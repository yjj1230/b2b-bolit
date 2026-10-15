// Package wecom —— 企微接入层（docs/07-wecom-integration.html；wrapper/plugin 隔离设计）
//
// ★ 多备案/多通道选型隔离点（用户要求）：
//
//	官方 API 与第三方 API 的选型推迟到最后时刻决定；本包是唯一消化该决策的薄层。
//	- 业务代码（api/workflows/repo）只依赖本包的 Gateway 接口，不感知通道实现；
//	- 通道实现按配置选择：mock（测试/演示，零外部依赖）| official（企微官方 API）|
//	  thirdparty（第三方 API，实现同一接口即可接入，无需改动业务代码）；
//	- 租户级配置（多备案切换，07 §6）由 TenantConfig 承载，按租户加载。
package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"b2b_pilot_v3/internal/config"
)

// ── 出站通道（S2 确认卡片 / S3 应用消息卡片 / S4 通知）──

// Card 出站卡片载荷（确认/催办/状态通知统一结构）。
type Card struct {
	TenantID   string          `json:"tenant_id"`
	OrderID    string          `json:"order_id,omitempty"`
	PreOrderID string          `json:"pre_order_id,omitempty"`
	Kind       string          `json:"kind"` // PRICE/CHAIRMAN/STOCK_WAREHOUSE/PAYMENT/WAREHOUSE/RECEIPT_EX/INVOICE_*/PREORDER_CONFIRM/ORDER_EVENT
	TaskID     string          `json:"task_id,omitempty"`
	Assignee   string          `json:"assignee,omitempty"`
	State      string          `json:"state,omitempty"`
	Candidates json.RawMessage `json:"candidates,omitempty"` // 预订单候选（确认卡片展示）
	Buttons    []CardButton    `json:"buttons,omitempty"`
}

// CardButton 卡片按钮（确认/修改/忽略；key 携带上下文，点击回调验签后转 Signal）。
type CardButton struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

// Outbound 出站通道契约（S2 确认卡片 / S3 通知卡片；07 §4）。
type Outbound interface {
	SendCard(ctx context.Context, card Card) (messageID string, err error)
}

// GroupSender 出站客户群消息通道（0.1 发群守门员 A3/N-01：确认后才允许发送）。
type GroupSender interface {
	SendGroupCard(ctx context.Context, tenantID, groupID string, card Card) (messageID string, err error)
}

// ── 入站通道（S2 群消息捕捉）──

// InboundMessage 会话消息（业务库只存意图特征 + archive_ref，原始文本不落库，07 §7）。
type InboundMessage struct {
	TenantID  string    `json:"tenant_id"`
	GroupID   string    `json:"group_id"`
	MsgID     string    `json:"msg_id"`
	SenderID  string    `json:"sender_id"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
	// ArchiveRef 存档引用（原始文本存独立加密可过期存档，业务库零留存）
	ArchiveRef string `json:"archive_ref"`
}

// Inbound 入站通道契约（官方 = 会话存档拉取循环；mock = 测试注入）。
type Inbound interface {
	// Pull 拉取一批消息（seq 游标由调用方持久化；官方实现封装 getchatdata+两段解密）。
	Pull(ctx context.Context, sinceSeq int64, limit int) ([]InboundMessage, int64, error)
}

// CallbackVerifier 回调验签（S3 点击回调；07 §5.3）。
type CallbackVerifier interface {
	// VerifyCallback 验签并解密回调；返回回调载荷（按钮 key / 消息上下文）。
	VerifyCallback(ctx context.Context, query, body []byte) (CallbackPayload, error)
	// VerifyURL 回调 URL 验证（GET echostr 解密，1 秒内响应）。
	VerifyURL(ctx context.Context, query []byte) ([]byte, error)
}

// CallbackPayload 回调载荷。
type CallbackPayload struct {
	TenantID   string `json:"tenant_id"`                          // 我方租户（路由填充，非企微 XML 字段）
	EventKey   string `json:"event_key" xml:"EventKey,omitempty"` // 按钮 key（task/pre_order 上下文）
	FromUserID string `json:"from_user_id" xml:"FromUserName,omitempty"`
	ToUserID   string `json:"to_user_id" xml:"ToUserName,omitempty"`
	MsgType    string `json:"msg_type" xml:"MsgType,omitempty"`
	MsgID      string `json:"msg_id" xml:"MsgId,omitempty"`
	CreateTime int64  `json:"create_time" xml:"CreateTime,omitempty"`
	// 0.1 A6：仓库群回单机器人（群图片消息回调；官方通道为 ChatId + PicUrl/MediaId）
	ChatID    string `json:"chat_id" xml:"ChatId,omitempty"`
	ImageURL  string `json:"image_url" xml:"PicUrl,omitempty"`
	ImageHash string `json:"image_hash"`
	MediaID   string `json:"media_id" xml:"MediaId,omitempty"` // 官方图片附件 ID（下载需 media/get；规则识别器优先用 PicUrl）
}

// ── Gateway：业务代码唯一依赖的聚合接口（wrapper/plugin 隔离点）──

// Gateway 聚合出站 + 入站 + 回调验签。
type Gateway interface {
	Outbound
	Inbound
	CallbackVerifier
	Name() string // mock | official | thirdparty
}

// NewGateway 按配置构造通道（B2B_WECOM_PROVIDER；thirdparty 为预留分支）。
func NewGateway(cfg config.Config) (Gateway, error) {
	switch cfg.WeComProvider {
	case "", "mock":
		return NewMockGateway(), nil
	case "official":
		return NewOfficialGateway(cfg)
	case "thirdparty":
		return nil, fmt.Errorf("provider=thirdparty 尚未接入：实现 wecom.Gateway 接口后在此注册（wrapper 隔离点）")
	default:
		return nil, fmt.Errorf("unknown wecom provider %q", cfg.WeComProvider)
	}
}

// ── 租户级配置（多备案切换，07 §6.2；存 DB 或加密配置文件）──

// TenantConfig 单租户企微配置。
type TenantConfig struct {
	TenantID string `json:"tenant_id"`
	CorpID   string `json:"corp_id"`
	Domain   string `json:"domain"` // ICP 备案域名（回调域名隔离）
	App      struct {
		AgentID        int    `json:"agent_id"`
		Secret         string `json:"secret"`
		Token          string `json:"token"`
		EncodingAESKey string `json:"encoding_aes_key"`
	} `json:"app"`
	Archive struct {
		Enabled      bool     `json:"enabled"`
		PublicKeyVer int      `json:"publickey_ver"`
		PrivateKeys  []string `json:"private_keys"` // 按 publickey_ver 保留历史私钥（07 §2.3）
		Secret       string   `json:"secret"`
	} `json:"archive"`
}

// MockOutbound 供测试断言用的出站记录。
type MockOutbound interface {
	Sent() []Card
}
