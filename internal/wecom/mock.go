package wecom

import (
	"context"
	"encoding/json"
	"sync"
)

// ── MockGateway：测试/演示通道（HTTP API + 企微 mock 模式默认；零外部依赖）──
// 出站卡片内存记录；入站消息由测试/HTTP 注入（模拟会话存档）；回调验签恒通过
// （验签正确性由官方实现与 W1 门禁覆盖，07 §8.1）。

// MockGateway 内存网关。
type MockGateway struct {
	mu         sync.Mutex
	cards      []Card
	groupCards []GroupCardRecord
	msgs       []InboundMessage
	seq        int64
}

// GroupCardRecord 客户群出站记录（守门员 N-01 测试断言：确认前不发送）。
type GroupCardRecord struct {
	TenantID string `json:"tenant_id"`
	GroupID  string `json:"group_id"`
	Card     Card   `json:"card"`
}

// NewMockGateway 构造。
func NewMockGateway() *MockGateway {
	return &MockGateway{}
}

// Name 通道名。
func (m *MockGateway) Name() string { return "mock" }

// SendCard 记录出站卡片（S3/S2 确认卡片）。
func (m *MockGateway) SendCard(ctx context.Context, card Card) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cards = append(m.cards, card)
	return "mock-msg-" + card.Kind + "-" + itoa(len(m.cards)), nil
}

// Sent 已发送卡片（测试断言）。
func (m *MockGateway) Sent() []Card {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Card, len(m.cards))
	copy(out, m.cards)
	return out
}

// SendGroupCard 记录客户群出站（守门员确认后才允许调用；测试断言发送时机）。
func (m *MockGateway) SendGroupCard(ctx context.Context, tenantID, groupID string, card Card) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groupCards = append(m.groupCards, GroupCardRecord{TenantID: tenantID, GroupID: groupID, Card: card})
	return "mock-group-msg-" + itoa(len(m.groupCards)), nil
}

// SentGroupCards 已发送客户群卡片（守门员测试断言）。
func (m *MockGateway) SentGroupCards() []GroupCardRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]GroupCardRecord, len(m.groupCards))
	copy(out, m.groupCards)
	return out
}

// Inject 注入一条入站消息（模拟会话存档拉到消息；S2 mock）。
func (m *MockGateway) Inject(ctx context.Context, msg InboundMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	m.seq++
	return nil
}

// Pull 返回注入的消息（sinceSeq 之后的）。
func (m *MockGateway) Pull(ctx context.Context, sinceSeq int64, limit int) ([]InboundMessage, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]InboundMessage, 0, len(m.msgs))
	for i, msg := range m.msgs {
		if int64(i) >= sinceSeq {
			out = append(out, msg)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, m.seq, nil
}

// VerifyCallback mock 验签：body 为 JSON CallbackPayload 时直接透传。
func (m *MockGateway) VerifyCallback(ctx context.Context, query, body []byte) (CallbackPayload, error) {
	var p CallbackPayload
	if err := jsonUnmarshal(body, &p); err != nil {
		return p, err
	}
	return p, nil
}

// VerifyURL mock：query 内 echo 原样返回（测试用）。
func (m *MockGateway) VerifyURL(ctx context.Context, query []byte) ([]byte, error) {
	return query, nil
}

// ── 通知适配（workflows.Notifier 契约 → 出站卡片）──

// Notifier 把业务通知映射为出站卡片（api/worker 注入用）。
type Notifier struct{ GW Gateway }

// NewNotifier 构造。
func NewNotifier(gw Gateway) *Notifier { return &Notifier{GW: gw} }

// NotifyApproval 审批待办/催办卡片（S3）：通过/驳回双按钮，点击回调分别投递
// approve:<taskID> / reject:<taskID>（按钮文案与动作语义一致，防止误点即通过）。
func (n *Notifier) NotifyApproval(ctx context.Context, tenantID, orderID, kind, assignee, taskID string) error {
	_, err := n.GW.SendCard(ctx, Card{
		TenantID: tenantID, OrderID: orderID, Kind: kind, TaskID: taskID, Assignee: assignee,
		Buttons: []CardButton{
			{Text: "通过", Key: "approve:" + taskID},
			{Text: "驳回", Key: "reject:" + taskID},
		},
	})
	return err
}

// NotifyPreOrderConfirm 预订单确认卡片（S2；确认/修改/忽略三按钮）。
func (n *Notifier) NotifyPreOrderConfirm(ctx context.Context, tenantID, preOrderID string, candidates []byte) error {
	_, err := n.GW.SendCard(ctx, Card{
		TenantID: tenantID, PreOrderID: preOrderID, Kind: "PREORDER_CONFIRM",
		Candidates: candidates,
		Buttons: []CardButton{
			{Text: "确认", Key: "preorder:confirm:" + preOrderID},
			{Text: "修改", Key: "preorder:modify:" + preOrderID},
			{Text: "忽略", Key: "preorder:ignore:" + preOrderID},
		},
	})
	return err
}

// NotifyOrderEvent 状态通知卡片（S3）。
func (n *Notifier) NotifyOrderEvent(ctx context.Context, tenantID, orderID, state string) error {
	_, err := n.GW.SendCard(ctx, Card{
		TenantID: tenantID, OrderID: orderID, Kind: "ORDER_EVENT", State: state,
	})
	return err
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
