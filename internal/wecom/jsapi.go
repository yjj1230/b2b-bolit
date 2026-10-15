package wecom

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── S1 侧边栏 H5 服务端能力（07 §6.1；对齐 v2 apps/wecom-miniapp + adapters）──
//
// 三入口分流 + OAuth snsapi_base + 双票据签名（ww.register 企业签名 / wx.agentConfig
// 应用签名）由前端承担（参考 v2 apps/wecom-miniapp/src/services/wecom-jssdk.ts）；
// 本文件实现服务端能力：① jsapi-config 双签名端点；② code → 身份/上下文交换
// （服务端重新授权身份，不信任前端传入身份——07 §6.1）；③ 上下文签发与校验。

// JSAPIConfig 双票据签名配置（wx.config + wx.agentConfig）。
type JSAPIConfig struct {
	CorpID  string `json:"corp_id"`
	AgentID string `json:"agent_id"`
	URL     string `json:"url"`
	// wx.config（企业 jsapi_ticket 签名）
	AppID     string `json:"app_id"`
	Timestamp int64  `json:"timestamp"`
	NonceStr  string `json:"nonce_str"`
	Signature string `json:"signature"`
	// wx.agentConfig（应用 agent_config ticket 签名；自建应用必须）
	AgentSignature string `json:"agent_signature"`
	AgentTimestamp int64  `json:"agent_timestamp"`
	AgentNonceStr  string `json:"agent_nonce_str"`
}

// JSAPIConfigBuilder 构建双签名配置。
// 本地/演示模式（mock）：确定性假签名（对齐 v2 FakeWeCom.BuildJSAPIConfig），
// 供前端联调与 E2E 断言；official 模式：真实 jsapi_ticket / agent_config ticket。
type JSAPIConfigBuilder struct {
	corpID   string
	agentID  string
	secret   string // mock 签名种子（确定性）
	domain   string // official 可信域名（备案域名；测试/生产环境注入不同值）
	official *OfficialGateway
}

// NewJSAPIConfigBuilder 构造 mock builder（corpID/agentID 来自租户配置；secret 为 mock 种子）。
func NewJSAPIConfigBuilder(corpID, agentID, secret string) *JSAPIConfigBuilder {
	return &JSAPIConfigBuilder{corpID: corpID, agentID: agentID, secret: secret}
}

// NewOfficialJSAPIConfigBuilder 构造 official builder。
// corp_id / agent_id / domain 全部来自 B2B_WECOM_TENANT_CFG 注入（测试/生产环境各自配置）。
func NewOfficialJSAPIConfigBuilder(gw *OfficialGateway) (*JSAPIConfigBuilder, error) {
	cfg, err := gw.TenantConfig()
	if err != nil {
		return nil, err
	}
	if cfg.CorpID == "" || cfg.App.AgentID <= 0 {
		return nil, fmt.Errorf("official jsapi: tenant config 缺 corp_id/agent_id")
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("official jsapi: tenant config 缺 domain（备案域名）")
	}
	return &JSAPIConfigBuilder{
		corpID: cfg.CorpID, agentID: strconv.Itoa(cfg.App.AgentID),
		domain: cfg.Domain, official: gw,
	}, nil
}

// OAuthURL 企微 snsapi_base 静默授权 URL（工作台 H5 OAuth 自举入口；07 §6.1）。
// redirect_uri 固定指向 {domain}/wecom/assistant（nginx 路由 → api /assistant，回跳带 code）。
// 企业微信自建应用 OAuth：appid=corpid，scope=snsapi_base 静默拿 userid。
func (b *JSAPIConfigBuilder) OAuthURL(state string) string {
	redirect := "https://" + b.domain + "/wecom/assistant"
	return "https://open.weixin.qq.com/connect/oauth2/authorize?appid=" + url.QueryEscape(b.corpID) +
		"&redirect_uri=" + url.QueryEscape(redirect) +
		"&response_type=code&scope=snsapi_base&state=" + url.QueryEscape(state) + "#wechat_redirect"
}

// BuildJSAPIConfig 生成双签名配置。url 仅允许 http/https（协议白名单，对齐 v2
// handleJSAPIConfig）；签名 = sha1(sort(ticket, noncestr, timestamp, url) 拼接)。
func (b *JSAPIConfigBuilder) BuildJSAPIConfig(ctx context.Context, pageURL string) (*JSAPIConfig, error) {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("jsapi url 协议白名单拒绝: %q", pageURL)
	}
	if b.official != nil && b.domain != "" {
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host != strings.ToLower(b.domain) && !strings.HasSuffix(host, "."+strings.ToLower(b.domain)) {
			return nil, fmt.Errorf("jsapi url 域名 %q 不在备案域名 %q 白名单", u.Hostname(), b.domain)
		}
	}
	now := time.Now().Unix()
	nonce := fmt.Sprintf("s1-%x", hmacSum(b.secret, fmt.Sprintf("nonce:%d", now)))
	var jsapiTicket, agentTicket string
	if b.official != nil {
		if jsapiTicket, err = b.official.JSAPITicket(ctx); err != nil {
			return nil, err
		}
		if agentTicket, err = b.official.AgentConfigTicket(ctx); err != nil {
			return nil, err
		}
		nonce = fmt.Sprintf("s1-%x", hmacSum(b.secret, fmt.Sprintf("official:%d", now)))
	} else {
		jsapiTicket, agentTicket = b.secret, b.secret
	}
	// 企业票据签名（wx.config）
	sig := jsapiSign(jsapiTicket, nonce, now, pageURL)
	// 应用票据签名（wx.agentConfig）
	agentSig := jsapiSign(agentTicket, nonce, now, pageURL)
	return &JSAPIConfig{
		CorpID: b.corpID, AgentID: b.agentID, URL: pageURL,
		AppID: b.corpID, Timestamp: now, NonceStr: nonce, Signature: sig,
		AgentSignature: agentSig, AgentTimestamp: now, AgentNonceStr: nonce,
	}, nil
}

// jsapiSign 企微签名公式：sha1(sort(ticket, noncestr, timestamp, url) 拼接)。
// mock 模式 ticket = 构造种子；official 模式 ticket = 企微真实 jsapi/agent_config ticket。
func jsapiSign(ticket, nonce string, ts int64, pageURL string) string {
	parts := []string{ticket, nonce, fmt.Sprintf("%d", ts), pageURL}
	sort.Strings(parts)
	h := sha1.New()
	h.Write([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(h.Sum(nil))
}

func hmacSum(key, msg string) []byte {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// ── 身份/上下文交换（POST /api/v1/auth/exchange）──

// ExchangedContext 交换结果（S1 前端 → 后续请求携带）。
type ExchangedContext struct {
	PrincipalID string `json:"principal_id"`
	TenantID    string `json:"tenant_id"` // "tenant:"+corpID（v2 语义）
	CustomerID  string `json:"customer_id"`
	// ContextToken 上下文令牌：服务端签发（HMAC），前端经 X-WeCom-Context 回传；
	// 服务端校验签名与 principal 一致性（07 §6.1：不信任前端传入身份）。
	ContextToken   string `json:"context_token"`
	CustomerManual bool   `json:"customer_manual"`
}

// IdentityProvider code → 身份/上下文解析（mock 模式；official 接线点=ExchangeCode 真实实现）。
type IdentityProvider interface {
	// Exchange 用 OAuth code + 会话 chat_id 解析身份与客户上下文。
	Exchange(ctx context.Context, code, chatID string) (ExchangedContext, error)
}

// MockIdentityProvider 演示实现：内置 code 表（对齐 v2 FakeWeCom 4 个测试 code）
// + chat→customer 绑定（内置映射 + WECOM_CHAT_BINDINGS 环境变量追加，对齐 v2）。
type MockIdentityProvider struct {
	tenantID string
	codes    map[string]string // code → principalID
	chats    map[string]string // chatID → customerID
}

// NewMockIdentityProvider 构造；codes 为 code→principal 表；chats 为 chat→customer 表
// （可被 WECOM_CHAT_BINDINGS="chat1=cust-0001,chat2=cust-0002" 环境变量追加）。
func NewMockIdentityProvider(tenantID string, codes, chats map[string]string) *MockIdentityProvider {
	all := map[string]string{}
	for k, v := range chats {
		all[k] = v
	}
	for _, pair := range strings.Split(os.Getenv("WECOM_CHAT_BINDINGS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 && kv[0] != "" {
			all[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return &MockIdentityProvider{tenantID: tenantID, codes: codes, chats: all}
}

// Exchange 解析 code + chat 上下文；未知 code / 未绑定 chat → 明确错误。
func (m *MockIdentityProvider) Exchange(ctx context.Context, code, chatID string) (ExchangedContext, error) {
	principal, ok := m.codes[code]
	if !ok {
		return ExchangedContext{}, fmt.Errorf("exchange: 未知 code %q", code)
	}
	customer, manual := "", false
	if chatID != "" {
		c, bound := m.chats[chatID]
		if !bound {
			return ExchangedContext{}, fmt.Errorf("exchange: chat %q 未绑定客户", chatID)
		}
		customer, manual = c, true
	}
	return ExchangedContext{
		PrincipalID: principal, TenantID: m.tenantID, CustomerID: customer,
		CustomerManual: manual,
	}, nil
}

// ── 上下文令牌（X-WeCom-Context 服务端签发与校验）──

// ContextIssuer 签发/校验 S1 上下文令牌（HMAC-SHA256，含过期）。
type ContextIssuer struct {
	secret []byte
	ttl    time.Duration
}

// NewContextIssuer 构造；secret 来自配置（WECOM_CONTEXT_SECRET，缺省开发密钥）。
func NewContextIssuer(secret string, ttl time.Duration) *ContextIssuer {
	if secret == "" {
		secret = "b2b-pilot-dev-context-secret"
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &ContextIssuer{secret: []byte(secret), ttl: ttl}
}

type contextClaim struct {
	PrincipalID string `json:"p"`
	TenantID    string `json:"t"`
	CustomerID  string `json:"c"`
	ExpNano     int64  `json:"e"` // 纳秒精度过期（避免秒边界同秒判定陷阱）
}

// Issue 签发令牌。
func (i *ContextIssuer) Issue(principalID, tenantID, customerID string) (string, error) {
	claim := contextClaim{
		PrincipalID: principalID, TenantID: tenantID, CustomerID: customerID,
		ExpNano: time.Now().Add(i.ttl).UnixNano(),
	}
	payload, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := hmacSum(string(i.secret), b64)
	return b64 + "." + hex.EncodeToString(sig), nil
}

// Verify 校验令牌，返回身份/上下文；签名失败或过期 → ok=false。
func (i *ContextIssuer) Verify(token string) (principalID, tenantID, customerID string, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	expect := hex.EncodeToString(hmacSum(string(i.secret), parts[0]))
	if !hmac.Equal([]byte(expect), []byte(parts[1])) {
		return "", "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", "", false
	}
	var c contextClaim
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", "", "", false
	}
	if time.Now().UnixNano() > c.ExpNano {
		return "", "", "", false
	}
	return c.PrincipalID, c.TenantID, c.CustomerID, true
}
