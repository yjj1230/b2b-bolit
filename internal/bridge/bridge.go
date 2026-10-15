// Package bridge —— v0.1 销售助手下单桥接（docs/发布计划v2 第 1 次发布 · A1 方案）。
//
// 目标模型：v0.1 只做「下单入口 + 订单列表/状态视图」，订单写进旧飞书三方审批系统，
// 后续审批与备货/发货/开票由飞书流全权续跑。本包负责：
//  1. 代理飞书 OAuth：构造授权 URL；code 交旧系统既有 /api/approval/feishu-user-by-code
//     换 create-order token（2h 有效期）；
//  2. token 缓存（按用户，TTL 2h，过期前 15 分钟视为需重授权）；
//  3. 调旧系统既有 POST /external-orders/create（multipart + 303 PRG）下单，
//     再轮询 GET /external-orders/submission-status 取 flow_id/instance_id；
//  4. 错误分类（FEISHU_AUTH_EXPIRED / ORDER_DATA_INVALID / SERVICE_MISCONFIGURED 等）。
//
// 旧系统契约（tripartite-approval @ f20619cc）：
//   - 幂等键前缀 B2B_V01_ 由旧系统识别（唯一代码小改）；
//   - 敏感边界：view_token/open_id 不落业务库；本包只返回安全字段。
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FeishuAuthorizeURL 飞书 OAuth 授权端点（旧系统应用）。
const FeishuAuthorizeURL = "https://open.feishu.cn/open-apis/authen/v1/authorize"

// IdempotencyPrefix 旧系统识别 v0.1 订单的幂等键前缀（D3 决策，唯一代码小改使用）。
const IdempotencyPrefix = "B2B_V01_"

// TokenTTL create-order token 有效期（旧系统为 2h）。
const TokenTTL = 2 * time.Hour

// ReauthWindow 过期前视为“需要重新授权”的窗口。
const ReauthWindow = 15 * time.Minute

// Config 桥接配置。
type Config struct {
	BaseURL     string       // 旧系统 tripartite-approval 地址
	AppID       string       // 旧系统飞书应用 app_id
	RedirectURI string       // v0.1 授权回调地址
	HTTPClient  *http.Client // 测试可注入；nil 时使用 15s 超时
	Cache       TokenCache   // 测试可注入；nil 时使用内存缓存
	Recorder    Recorder     // 下单成功回写 v0.1 PG；nil 时跳过（测试）
}

// Client 桥接客户端。
type Client struct {
	cfg   Config
	http  *http.Client
	cache TokenCache
	save  Recorder
}

// New 构造桥接客户端；BaseURL/AppID/RedirectURI 必填。
func New(cfg Config) (*Client, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("bridge: base URL is required")
	}
	if cfg.AppID == "" || cfg.RedirectURI == "" {
		return nil, fmt.Errorf("bridge: app id and redirect uri are required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	cache := cfg.Cache
	if cache == nil {
		cache = NewMemoryTokenCache()
	}
	return &Client{cfg: cfg, http: httpClient, cache: cache, save: cfg.Recorder}, nil
}

// AuthorizationURL 构造飞书授权 URL。
func (c *Client) AuthorizationURL(state string) string {
	return FeishuAuthorizeURL + "?app_id=" + url.QueryEscape(c.cfg.AppID) +
		"&redirect_uri=" + url.QueryEscape(c.cfg.RedirectURI) +
		"&state=" + url.QueryEscape(state)
}

// TokenFor 读取当前用户缓存 token（未过期）。
func (c *Client) TokenFor(userID string) (string, time.Time, bool) {
	return c.cache.Get(userID, time.Now())
}

// Identity 授权换回的飞书身份（open_id 不入业务库）。
type Identity struct {
	UserID  string `json:"user_id"`
	OpenID  string `json:"open_id"`
	Name    string `json:"name"`
	UnionID string `json:"union_id"`
	Role    string `json:"role"` // sales | agent（旧系统语义）
}

// ExchangeResult code 换 token 结果。
type ExchangeResult struct {
	Token    string
	Identity Identity
}

// exchangeResponse 旧系统实际响应：{ok,user:{name,open_id,user_id,union_id},matched,role,token,items}。
type exchangeResponse struct {
	OK   bool `json:"ok"`
	User struct {
		Name    string `json:"name"`
		OpenID  string `json:"open_id"`
		UserID  string `json:"user_id"`
		UnionID string `json:"union_id"`
	} `json:"user"`
	Role    string `json:"role"`
	Token   string `json:"token"`
	Message string `json:"message"`
}

// ExchangeCode 用授权 code 换取 create-order token（经旧系统既有接口）。
func (c *Client) ExchangeCode(ctx context.Context, userID, code string) (*ExchangeResult, error) {
	if code == "" {
		return nil, &Error{Kind: ErrAuthExpired, Message: "empty oauth code"}
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/api/approval/feishu-user-by-code", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("bridge: exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge: exchange code: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("bridge: read exchange response: %w", err)
	}
	var env exchangeResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bridge: decode exchange response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadGateway {
		return nil, &Error{Kind: ErrAuthExpired, Message: firstNonEmpty(env.Message, "exchange failed")}
	}
	if !env.OK {
		if resp.StatusCode == http.StatusForbidden {
			return nil, &Error{Kind: ErrAuthExpired, Message: firstNonEmpty(env.Message, "user not in whitelist")}
		}
		return nil, &Error{Kind: ErrUnknown, Message: firstNonEmpty(env.Message, "exchange failed")}
	}
	if env.Token == "" {
		return nil, &Error{Kind: ErrAuthExpired, Message: "exchange response missing token"}
	}
	id := Identity{UserID: env.User.UserID, OpenID: env.User.OpenID, Name: env.User.Name, UnionID: env.User.UnionID, Role: env.Role}
	_ = c.cache.Put(userID, env.Token, time.Now().Add(TokenTTL))
	// 同时挂到服务端返回的真实身份键下：授权回调请求的 X-User-Id 可能为空（前端未回填），
	// 而下单时以 identity.user_id 作为 X-User-Id 查询；双键确保两种场景都能命中。
	_ = c.cache.Put(id.UserID, env.Token, time.Now().Add(TokenTTL))
	return &ExchangeResult{Token: env.Token, Identity: id}, nil
}

// Item 订单明细（旧系统 items_json 项；无 total 字段，由旧系统计算）。
type Item struct {
	ProductName string  `json:"product_name"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"` // 元
	Packaging   string  `json:"packaging,omitempty"`
	PackageSpec string  `json:"package_spec,omitempty"`
	ArrivalDate string  `json:"arrival_date,omitempty"`
	Remark      string  `json:"remark,omitempty"`
}

// OrderInput v0.1 表单 → 旧系统 SubmitOrderInput 的桥接字段。
type OrderInput struct {
	TenantID           string
	OrderID            string // 生成 B2B_V01_<tenant>:<orderID> 幂等键
	CustomerRecordID   string // 老客户 Bitable record_id（非空走 EXISTING）
	CustomerName       string // 新客户名称 / 老客户展示名
	CustomerLevel      string
	TaxNo              string
	Supplier           string
	Items              []Item
	PaymentTerms       string
	RemittanceMethod   string
	Currency           string
	IsSelfPickup       bool
	PickupWarehouse    string
	ShippingAddress    string
	Receiver           string
	Phone              string
	CustomerContractNo string
	OrderRemark        string
	SpecialSpec        string
	Sales              Identity // 归属销售（Bearer 身份校验）
	Agent              Identity // 代办人（角色 agent 时）
}

// IdempotencyKey 生成旧系统幂等键（前缀 B2B_V01_）。
func (in OrderInput) IdempotencyKey() string {
	id := in.OrderID
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return IdempotencyPrefix + in.TenantID + ":" + id
}

// Submission 下单成功回执（只含安全字段）。JSON 字段名与旧系统/前端契约一致（flow_id/instance_id/...）。
type Submission struct {
	FlowID     string `json:"flow_id"`
	InstanceID string `json:"instance_id"`
	StageID    string `json:"stage_id"`
	DetailURL  string `json:"detail_url"`
}

// createRedirectResponse 旧系统 303 Location 可能带 created=1&submitter_token=...。
func parseInstanceIDFromLocation(loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "external-orders" {
		return parts[1]
	}
	return u.Query().Get("instance_id")
}

// CreateOrder 调旧系统 POST /external-orders/create（multipart），再轮询 submission-status。
func (c *Client) CreateOrder(ctx context.Context, token string, in OrderInput) (*Submission, error) {
	if token == "" {
		return nil, &Error{Kind: ErrAuthExpired, Message: "missing create-order token"}
	}
	itemsJSON, err := json.Marshal(in.Items)
	if err != nil {
		return nil, fmt.Errorf("bridge: marshal items: %w", err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	writeField := func(k, v string) {
		if v != "" {
			_ = mw.WriteField(k, v)
		}
	}
	writeField("create_idempotency_key", in.IdempotencyKey())
	if in.CustomerRecordID != "" {
		writeField("customer_mode", "EXISTING")
		writeField("customer_existing_record_id", in.CustomerRecordID)
		writeField("customer_existing_name", in.CustomerName)
	} else {
		writeField("customer_mode", "NEW")
		writeField("customer_new_name", in.CustomerName)
	}
	writeField("items_json", string(itemsJSON))
	writeField("supplier", in.Supplier)
	writeField("customer_level", in.CustomerLevel)
	writeField("tax_no", in.TaxNo)
	writeField("payment_terms", in.PaymentTerms)
	writeField("remittance_method", in.RemittanceMethod)
	writeField("currency", firstNonEmpty(in.Currency, "CNY"))
	selfPickup := "否"
	if in.IsSelfPickup {
		selfPickup = "是"
	}
	writeField("is_self_pickup", selfPickup)
	if in.IsSelfPickup {
		writeField("pickup_warehouse", in.PickupWarehouse)
	} else {
		writeField("shipping_address", in.ShippingAddress)
		writeField("receiver", in.Receiver)
		writeField("phone", in.Phone)
	}
	writeField("customer_contract_no", in.CustomerContractNo)
	writeField("order_remark", in.OrderRemark)
	writeField("special_spec_requirements", in.SpecialSpec)
	writeField("sales_name", in.Sales.Name)
	writeField("sales_open_id", in.Sales.OpenID)
	writeField("sales_user_id", in.Sales.UserID)
	if in.Sales.Role != "" {
		writeField("sales_role_key", in.Sales.Role)
	}
	if in.Agent.OpenID != "" || in.Agent.UserID != "" {
		writeField("agent_name", in.Agent.Name)
		writeField("agent_open_id", in.Agent.OpenID)
		writeField("agent_user_id", in.Agent.UserID)
		if in.Agent.Role != "" {
			writeField("agent_role_key", in.Agent.Role)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("bridge: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/external-orders/create", &body)
	if err != nil {
		return nil, fmt.Errorf("bridge: create order request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// 旧系统成功是 303 重定向；失败返回 JSON。不下跟随重定向，避免丢失 Location。
	httpc := *c.http
	httpc.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge: create order: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, classifyStatus(resp.StatusCode, raw)
	case resp.StatusCode == http.StatusConflict:
		return nil, &Error{Kind: ErrIdempotencyConflict, Message: firstNonEmpty(msgOf(raw), "idempotency conflict")}
	case resp.StatusCode >= 400:
		return nil, classifyStatus(resp.StatusCode, raw)
	}

	sub := &Submission{InstanceID: parseInstanceIDFromLocation(resp.Header.Get("Location"))}
	if sub.InstanceID == "" {
		return nil, &Error{Kind: ErrUnknown, Message: "create redirect missing instance_id"}
	}
	// 轮询既有 submission-status（Bearer），补 flow_id/stage_id/detail_url。
	for attempt := 0; attempt < 30; attempt++ {
		select {
		case <-ctx.Done():
			return sub, ctx.Err()
		case <-time.After(time.Second):
		}
		st, err := c.submissionStatus(ctx, token, in.IdempotencyKey())
		if err != nil {
			continue
		}
		if st.FlowID != "" {
			sub.FlowID = st.FlowID
			sub.DetailURL = st.DetailURL
		}
		if st.InstanceID != "" {
			sub.InstanceID = st.InstanceID
		}
		if sub.FlowID != "" && sub.InstanceID != "" {
			if c.save != nil {
				if err := c.save.RecordSubmission(ctx, in.TenantID, in.OrderID, sub.FlowID, sub.InstanceID, sub.StageID, sub.DetailURL); err != nil {
					return nil, fmt.Errorf("bridge: record submission: %w", err)
				}
			}
			return sub, nil
		}
	}
	return sub, nil
}

type submissionStatusResponse struct {
	State      string `json:"state"`
	OrderID    string `json:"order_id"`
	InstanceID string `json:"instance_id"`
	FlowID     string `json:"flow_id"`
	DetailURL  string `json:"detail_url"`
	StageID    string `json:"stage_id"`
}

func (c *Client) submissionStatus(ctx context.Context, token, key string) (*submissionStatusResponse, error) {
	u := c.cfg.BaseURL + "/external-orders/submission-status?key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("submission-status: %d", resp.StatusCode)
	}
	var st submissionStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func msgOf(raw []byte) string {
	var m struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		OK      bool   `json:"ok"`
	}
	_ = json.Unmarshal(raw, &m)
	return firstNonEmpty(m.Message, m.Error)
}

// SalesPerson 可选归属销售（旧系统 /sales-persons 返回；仅代办角色可查）。
type SalesPerson struct {
	Name    string `json:"name"`
	UserID  string `json:"user_id"`
	OpenID  string `json:"open_id"`
	RoleKey string `json:"role_key"`
}

// ListSalesPersons 拉取当前用户可选归属销售列表（带 create-order token 调旧系统）。
func (c *Client) ListSalesPersons(ctx context.Context, token string) ([]SalesPerson, error) {
	if token == "" {
		return nil, &Error{Kind: ErrAuthExpired, Message: "missing create-order token"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/api/approval/sales-persons", nil)
	if err != nil {
		return nil, fmt.Errorf("bridge: sales persons request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge: sales persons: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, classifyStatus(resp.StatusCode, raw)
	}
	var env struct {
		OK    bool          `json:"ok"`
		Items []SalesPerson `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bridge: decode sales persons: %w", err)
	}
	return env.Items, nil
}

func classifyStatus(status int, raw []byte) *Error {
	msg := msgOf(raw)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &Error{Kind: ErrAuthExpired, Message: firstNonEmpty(msg, "feishu auth failed")}
	}
	return classifyError(status, firstNonEmpty(msg, "order create failed"))
}

// ResolveExistingCustomerRecord 老客户 ID 交叉映射（T1.3/HR2-2c）：
// 新系统 pgAPI 客户 record_id 为 api_*，旧系统 bitable 客户 record_id 为 rec*。
// 下单前按 客户名 + 销售员 open_id 调旧系统 /api/bitable/customers/search，
// 精确匹配客户名取旧系统 rec id；未找到返回空串。
func (c *Client) ResolveExistingCustomerRecord(ctx context.Context, token, customerName, salesOpenID string) (string, error) {
	if strings.TrimSpace(customerName) == "" || strings.TrimSpace(salesOpenID) == "" {
		return "", nil
	}
	u := c.cfg.BaseURL + "/api/bitable/customers/search?keyword=" + url.QueryEscape(strings.TrimSpace(customerName)) +
		"&sales_open_id=" + url.QueryEscape(strings.TrimSpace(salesOpenID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("bridge: resolve customer request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("bridge: resolve customer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", classifyStatus(resp.StatusCode, raw)
	}
	var env struct {
		OK    bool `json:"ok"`
		Items []struct {
			RecordID     string `json:"record_id"`
			CustomerName string `json:"customer_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("bridge: decode customer resolve: %w", err)
	}
	target := strings.TrimSpace(customerName)
	for _, it := range env.Items {
		if strings.TrimSpace(it.CustomerName) == target {
			return it.RecordID, nil
		}
	}
	return "", nil
}

// TokenCache token 缓存契约。
type TokenCache interface {
	Get(userID string, now time.Time) (token string, expiresAt time.Time, ok bool)
	Put(userID, token string, expiresAt time.Time) error
}

// MemoryTokenCache 内存缓存（单实例演示/测试）。
type MemoryTokenCache struct {
	mu    sync.Mutex
	items map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewMemoryTokenCache 构造内存缓存。
func NewMemoryTokenCache() *MemoryTokenCache {
	return &MemoryTokenCache{items: map[string]cachedToken{}}
}

// Get 返回未过期的 token。
func (m *MemoryTokenCache) Get(userID string, now time.Time) (string, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[userID]
	if !ok || now.After(it.expiresAt) {
		return "", time.Time{}, false
	}
	return it.token, it.expiresAt, true
}

// Put 写入 token。
func (m *MemoryTokenCache) Put(userID, token string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[userID] = cachedToken{token: token, expiresAt: expiresAt}
	return nil
}

// Recorder 下单成功回写 v0.1 PG 的契约（api 层注入 repo 实现）。
type Recorder interface {
	RecordSubmission(ctx context.Context, tenantID, orderID, flowID, instanceID, stageID, detailURL string) error
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
