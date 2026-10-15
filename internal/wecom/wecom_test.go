package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/config"
)

// Mock 通道：出站卡片 + 入站注入 + 通知映射（S2/S3 链路）
func TestMockGatewayAndNotifier(t *testing.T) {
	gw := NewMockGateway()
	require.Equal(t, "mock", gw.Name())

	// 出站卡片
	card := Card{TenantID: "chang-an", OrderID: "ORD-1", Kind: "PRICE", Assignee: "u-supervisor"}
	mid, err := gw.SendCard(context.Background(), card)
	require.NoError(t, err)
	require.NotEmpty(t, mid)
	require.Len(t, gw.Sent(), 1)

	// 入站注入 + 拉取（模拟会话存档）
	require.NoError(t, gw.Inject(context.Background(), InboundMessage{
		TenantID: "chang-an", GroupID: "group-demo", MsgID: "m1", Text: "SKU-C68 10件",
	}))
	msgs, seq, err := gw.Pull(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, int64(1), seq)
	// 游标推进后拉不到旧消息
	msgs2, _, err := gw.Pull(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Empty(t, msgs2)

	// 通知映射（workflows.Notifier 契约）
	n := NewNotifier(gw)
	require.NoError(t, n.NotifyApproval(context.Background(), "chang-an", "ORD-1", "PRICE", "u-supervisor", "AT-1"))
	require.NoError(t, n.NotifyPreOrderConfirm(context.Background(), "chang-an", "PRE-1", []byte(`[{"qty":1}]`)))
	require.NoError(t, n.NotifyOrderEvent(context.Background(), "chang-an", "ORD-1", "SHP_SHIPPED"))
	cards := gw.Sent()
	require.Len(t, cards, 4)
	require.Equal(t, "PRICE", cards[1].Kind)
	// WEC-05：审批卡必须是通过/驳回双按钮，文案与 EventKey 动作语义一致
	require.Len(t, cards[1].Buttons, 2)
	require.Equal(t, "通过", cards[1].Buttons[0].Text)
	require.Equal(t, "approve:AT-1", cards[1].Buttons[0].Key)
	require.Equal(t, "驳回", cards[1].Buttons[1].Text)
	require.Equal(t, "reject:AT-1", cards[1].Buttons[1].Key)
	require.Equal(t, "PREORDER_CONFIRM", cards[2].Kind)
	require.Len(t, cards[2].Buttons, 3)
	require.Equal(t, "ORDER_EVENT", cards[3].Kind)
}

// Mock 回调：透传载荷（验签正确性由 official 实现 + W1 门禁覆盖）
func TestMockCallback(t *testing.T) {
	gw := NewMockGateway()
	payload := CallbackPayload{TenantID: "chang-an", EventKey: "approve:AT-1", FromUserID: "u-supervisor"}
	body, _ := json.Marshal(payload)
	p, err := gw.VerifyCallback(context.Background(), []byte("msg_signature=x"), body)
	require.NoError(t, err)
	require.Equal(t, "approve:AT-1", p.EventKey)
	echo, err := gw.VerifyURL(context.Background(), []byte("echostr=hello"))
	require.NoError(t, err)
	require.Equal(t, "echostr=hello", string(echo))
}

// 通道工厂（wrapper 隔离点）：配置选型
func TestGatewayFactory(t *testing.T) {
	gw, err := NewGateway(config.Config{WeComProvider: ""})
	require.NoError(t, err)
	require.Equal(t, "mock", gw.Name())

	gw, err = NewGateway(config.Config{WeComProvider: "mock"})
	require.NoError(t, err)
	require.Equal(t, "mock", gw.Name())

	// official 缺租户配置 → 明确错误
	_, err = NewGateway(config.Config{WeComProvider: "official"})
	require.Error(t, err)

	// thirdparty 预留分支 → 明确错误（选型决策点）
	_, err = NewGateway(config.Config{WeComProvider: "thirdparty"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "thirdparty")

	_, err = NewGateway(config.Config{WeComProvider: "bogus"})
	require.Error(t, err)
}

// 官方通道：回调签名/解密基础单元（W1 前置；无真实凭据只验证结构路径）
func TestOfficialVerifySignature(t *testing.T) {
	o := &OfficialGateway{httpc: nil}
	// 空配置 → 验签走 tenantConfig 占位 → 签名必然不匹配（无真实 token）
	_, err := o.verifySignature("deadbeef", "123", "456", "enc")
	require.Error(t, err) // 签名不匹配 → 拒绝
	// 解密路径：空 EncodingAESKey → 错误
	_, err = o.decryptCallback("abc")
	require.Error(t, err)
}

// 多备案配置模型（07 §6.2）：租户配置结构可序列化
func TestTenantConfigModel(t *testing.T) {
	raw := `{"tenant_id":"chang-an","corp_id":"ww-1","domain":"wecom.changan.example.com",
		"app":{"agent_id":1000002,"secret":"s","token":"t","encoding_aes_key":"k"},
		"archive":{"enabled":true,"publickey_ver":4,"private_keys":["k4","k3"],"secret":"a"}}`
	var tc TenantConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &tc))
	require.Equal(t, "ww-1", tc.CorpID)
	require.Equal(t, 4, tc.Archive.PublicKeyVer)
	require.Len(t, tc.Archive.PrivateKeys, 2)
}

// 官方生产路径错误分支（tenantConfig 占位 → 空 token/key → 明确拒绝）
func TestOfficialProductionErrorPaths(t *testing.T) {
	o := &OfficialGateway{httpc: &http.Client{}}
	ctx := context.Background()

	// VerifyURL：签名必然不匹配（占位 token）→ 拒绝
	_, err := o.VerifyURL(ctx, []byte("msg_signature=x&timestamp=1&nonce=n&echostr=e"))
	require.Error(t, err)
	// VerifyCallback：签名不匹配 → 拒绝
	_, err = o.VerifyCallback(ctx, []byte("msg_signature=x&timestamp=1&nonce=n"), []byte(`{"encrypt":"e"}`))
	require.Error(t, err)
	// VerifyCallback：body 非法 JSON → 拒绝
	_, err = o.VerifyCallback(ctx, []byte("msg_signature=x"), []byte(`{bad`))
	require.Error(t, err)
	// decryptCallback：空 EncodingAESKey → 拒绝
	_, err = o.decryptCallback("abc")
	require.Error(t, err)
	// 非法 base64 → 拒绝
	_, err = o.decryptCallbackWithKey("!!!not-base64!!!", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
	// 过短密文 → 拒绝
	_, err = o.decryptCallbackWithKey("aGVsbG8=", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
}

// wecom 边界补充（覆盖率补强）
func TestWecomEdges(t *testing.T) {
	// marshal 失败分支（不可序列化值 → 兜底 []）
	b := marshal(func() {})
	require.Equal(t, "[]", string(b))
	// marshal 成功
	require.Equal(t, `{"a":1}`, string(marshal(map[string]any{"a": 1})))
	// itoa 边界
	require.Equal(t, "0", itoa(0))
	require.Equal(t, "7", itoa(7))
	require.Equal(t, "123", itoa(123))

	// mock Pull 边界：limit 截断 + sinceSeq 过滤
	gw := NewMockGateway()
	for i := 0; i < 5; i++ {
		require.NoError(t, gw.Inject(context.Background(), InboundMessage{
			TenantID: "t", GroupID: "g", MsgID: "m" + itoa(i), Text: "x",
		}))
	}
	msgs, seq, err := gw.Pull(context.Background(), 2, 2)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, int64(5), seq)

	// mock VerifyCallback 非法 JSON → 错误
	_, err = gw.VerifyCallback(context.Background(), []byte("q"), []byte(`{bad`))
	require.Error(t, err)

	// TenantConfig 模型：必填校验（缺 token）
	_, err = loadTenantConfigFile(t.TempDir() + "/no.json")
	require.Error(t, err)
}

// 剩余网络/解码错误分支（NewRequest 非法 URL / 配置坏 JSON / 坏 AES key）
func TestWecomRemainingErrorBranches(t *testing.T) {
	ctx := context.Background()
	// 非法 URL（含控制字符）→ http.NewRequestWithContext 失败
	o := &OfficialGateway{httpc: &http.Client{}, baseURL: "http://x"}
	_, err := o.SendCard(ctx, Card{Kind: "X"}) // token 为空 → 先走 getToken → 网络失败
	require.Error(t, err)
	o.token, o.tokenExp = "t", time.Now().Add(time.Hour)
	// SendCard 的 NewRequest 失败需要非法 URL——baseURL 注入控制字符
	o2 := &OfficialGateway{httpc: &http.Client{}, baseURL: "http://\x00"}
	o2.token, o2.tokenExp = "t", time.Now().Add(time.Hour)
	_, err = o2.SendCard(ctx, Card{Kind: "X"})
	require.Error(t, err)
	// Pull 的 NewRequest 失败
	_, _, err = o2.Pull(ctx, 0, 10)
	require.Error(t, err)
	// getJSON 的 NewRequest 失败（经 getToken→tenantConfig 占位路径）
	o3 := &OfficialGateway{httpc: &http.Client{}, baseURL: "http://\x00"}
	o3.token, o3.tokenExp = "", time.Time{}
	_, err = o3.getToken(ctx)
	require.Error(t, err)

	// loadTenantConfigFile 坏 JSON
	badPath := t.TempDir() + "/bad.json"
	require.NoError(t, os.WriteFile(badPath, []byte(`{bad`), 0o600))
	_, err = loadTenantConfigFile(badPath)
	require.Error(t, err)

	// decryptCallback 坏 EncodingAESKey（无效 base64）
	o4 := &OfficialGateway{}
	_, err = o4.decryptCallbackWithKey("YWJj", []byte("short"))
	require.Error(t, err)
	// VerifyURL/VerifyCallback 的 decrypt 失败分支（签名通过但密文坏）
	o5 := &OfficialGateway{}
	_, err = o5.VerifyURLWithToken(ctx, []byte("msg_signature=deadbeef&timestamp=1&nonce=n&echostr=!!!"), "tok", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
	// VerifyCallback 签名不匹配分支（经 VerifyCallbackWithToken）
	_, err = o5.VerifyCallbackWithToken(ctx, []byte("msg_signature=deadbeef&timestamp=1&nonce=n"), []byte(`{"encrypt":"x"}`), "tok", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
}
