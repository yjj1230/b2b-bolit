package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/bridge"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

// ── HTTP API 服务（docs/02-architecture.html §2.1；M6）──
// 授权预检 → 幂等启动 Workflow → 审批动作（Signal）→ 查询（订单/事件链/待办/投影）。

// Starter 工作流启动/信号契约（Temporal 客户端或本地内嵌 runner；测试用 fakeStarter）。
type Starter interface {
	StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error)
	StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error)
	SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error
}

// LocalSalesPersonProvider supplies the local demo list when the Feishu bridge
// is disabled. Production requests continue to use bridge.Client instead.
type LocalSalesPersonProvider interface {
	ListSalesPersons(ctx context.Context, tenantID string) ([]bridge.SalesPerson, error)
}

// Server HTTP 服务。
type Server struct {
	Store   workflows.Store
	Starter Starter
	Checker authz.Checker
	RoleOf  func(userID string) (authz.Role, string) // userID → (role, tenantID)；演示模式从请求头解析
	// RoleResolver ROL-01：生产模式白名单身份解析（userID → role,tenant,ok）；未命中 → 401。
	// nil = 演示模式（沿用 RoleOf）。
	RoleResolver func(userID string) (authz.Role, string, bool)
	Logf         func(format string, args ...any)
	// S1 侧边栏 H5 服务端能力（07 §6.1；nil = 未启用）：
	//   JSAPI    双票据签名配置（wx.config + wx.agentConfig）
	//   Identity code → 身份/上下文交换（OAuth snsapi_base）
	//   Context  上下文令牌签发/校验（X-WeCom-Context 服务端校验）
	JSAPI    *wecom.JSAPIConfigBuilder
	Identity wecom.IdentityProvider
	Context  *wecom.ContextIssuer
	// GroupSender 出站客户群通道（0.1 守门员 A3：确认后才发送；nil = 不发送只留痕）
	GroupSender wecom.GroupSender
	// Bridge v0.1 销售助手下单桥接（docs/发布计划v2 第 1 次发布；nil = 未启用）
	Bridge *bridge.Client
	// LocalSalesPersons 本地演示模式的归属销售来源（通常为 PostgreSQL users 表）。
	LocalSalesPersons LocalSalesPersonProvider
	// BridgeLedger 桥接自建单台账查询（repo 实现；nil 时列表端点返回 503）
	BridgeLedger BridgeLedger
	// Data 订单数据实体 Provider（pgAPI；INV-01 发票抬头税号校验；nil 时跳过税号比对）
	Data orderdata.Provider
	// PGAPIArchiveEnabled 测试特化：新客户下单后直写备库 pgAPI（B2B_PGAPI_ARCHIVE_ENABLED=true；生产默认关）
	PGAPIArchiveEnabled bool
	// AuthStrict 生产严格身份（B2B_AUTH_STRICT=true）：withContext 只认 X-WeCom-Context，
	// 无令牌/校验失败一律 401；false=演示/测试模式（保留 X-User-Id 信任，业务流转不变）。
	AuthStrict bool
}

// BridgeLedger 桥接台账查询契约。
type BridgeLedger interface {
	ListBridgeSubmissions(ctx context.Context, tenantID string, limit int) ([]repo.BridgeSubmission, error)
}

type AgentTraceReader interface {
	GetAgentOrderTrace(ctx context.Context, tenantID, orderID string) (*repo.AgentOrderTrace, error)
}

// NewServer 构造服务。
func NewServer(store workflows.Store, starter Starter, checker authz.Checker, roleOf func(string) (authz.Role, string)) *Server {
	return &Server{Store: store, Starter: starter, Checker: checker, RoleOf: roleOf, Logf: log.Printf}
}

// Routes 注册路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/v1/orders", s.handleCreateOrder)
	mux.HandleFunc("GET /api/v1/orders", s.handleListOrders)
	mux.HandleFunc("POST /api/v1/orders/{orderID}/signal/{signal}", s.handleOrderSignal)
	mux.HandleFunc("GET /api/v1/orders/{orderID}", s.handleGetOrder)
	mux.HandleFunc("GET /api/v1/orders/{orderID}/shipments", s.handleListOrderShipments)
	mux.HandleFunc("GET /api/v1/orders/{orderID}/events", s.handleOrderEvents)
	mux.HandleFunc("GET /api/v1/todos", s.handleTodos)
	mux.HandleFunc("POST /api/v1/orders/{orderID}/shipments/{shipmentID}/evidence", s.handleAppendEvidence)
	mux.HandleFunc("GET /api/v1/orders/{orderID}/shipments/{shipmentID}/evidence", s.handleListEvidence)
	mux.HandleFunc("POST /api/v1/preorders", s.handleCreatePreOrder)
	mux.HandleFunc("POST /api/v1/preorders/{preOrderID}/signal/{signal}", s.handlePreOrderSignal)
	mux.HandleFunc("GET /api/v1/preorders/{preOrderID}", s.handleGetPreOrder)
	mux.HandleFunc("GET /api/v1/agent/orders/{orderID}/trace", s.handleAgentOrderTrace)
	// S1 侧边栏 H5（07 §6.1；对齐 v2 bff-api）：JS-SDK 双签名 + OAuth 身份/上下文交换
	mux.HandleFunc("GET /api/v1/wecom/jsapi-config", s.handleJSAPIConfig)
	mux.HandleFunc("GET /api/v1/wecom/oauth-url", s.handleWeComOAuthURL)
	mux.HandleFunc("POST /api/v1/auth/exchange", s.handleExchange)
	// 0.1 发布版新增端点（docs/v0.1/08-api.html §1：守门员/合并开票/三金额）
	mux.HandleFunc("POST /api/v1/orders/{orderID}/send-to-group", s.handleSendToGroup)
	mux.HandleFunc("POST /api/v1/invoices/{invoiceID}/link-orders", s.handleLinkInvoiceOrder)
	mux.HandleFunc("GET /api/v1/orders/{orderID}/invoice-summary", s.handleInvoiceSummary)
	// v0.1 销售助手下单桥接（A1：OAuth 授权 URL / code 换 token / 下单调用器）
	mux.HandleFunc("GET /api/v1/bridge/auth-url", s.handleBridgeAuthURL)
	mux.HandleFunc("POST /api/v1/bridge/callback", s.handleBridgeCallback)
	mux.HandleFunc("POST /api/v1/bridge/orders", s.handleBridgeCreateOrder)
	mux.HandleFunc("GET /api/v1/bridge/orders", s.handleBridgeListOrders)
	mux.HandleFunc("GET /api/v1/bridge/sales-persons", s.handleBridgeSalesPersons)
	// 极简 PC 下单/订单后台（企微备案域名未就绪时的兜底入口）
	mux.HandleFunc("GET /bridge", s.handleBridgeUI)
	mux.HandleFunc("GET /bridge/", s.handleBridgeUI)
	// 销售助理企微 H5 / PC 双端页面（工作台代下单 + 客户群入口代下单）
	mux.HandleFunc("GET /assistant", s.handleAssistantUI)
	mux.HandleFunc("GET /assistant/", s.handleAssistantUI)
	// UI-01：pgAPI 订单数据实体搜索（客户/产品/购销合同预填）
	mux.HandleFunc("GET /api/v1/orderdata/customers", s.handleSearchCustomers)
	mux.HandleFunc("GET /api/v1/orderdata/products", s.handleSearchProducts)
	mux.HandleFunc("GET /api/v1/orderdata/purchase-contracts", s.handleSearchContracts)
	// UI-02：商务最小操作台（待办裁决 + 凭证登记 + 全量订单）
	mux.HandleFunc("GET /ops", s.handleOpsUI)
	mux.HandleFunc("GET /ops/", s.handleOpsUI)
	return s.withContext(mux)
}

func (s *Server) handleAgentOrderTrace(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	reader, ok := s.Store.(AgentTraceReader)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "agent trace unavailable")
		return
	}
	trace, err := reader.GetAgentOrderTrace(r.Context(), u.TenantID, r.PathValue("orderID"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

// ── 请求上下文（演示模式：X-User-Id / X-Role 头；S1 侧边栏：X-WeCom-Context 服务端校验）──

type ctxUser struct{ UserID, Role, TenantID, CustomerID string }

func (s *Server) withContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 免鉴权公开路径：健康检查 + UI 页面 + OAuth 引导端点。
		// 生产严格模式下这些是入口（页面加载 / jsapi-config / code 换令牌），
		// 必须无令牌可访问；业务端点仍走 X-WeCom-Context + 白名单强校验。
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-WeCom-Context")
		if s.AuthStrict {
			// 生产严格身份（B2B_AUTH_STRICT=true）：只认服务端签发的企微上下文令牌。
			// 无令牌 / 校验失败 / 不在白名单 → 一律 401；不信任前端 X-User-Id。
			// 测试环境 AuthStrict=false 保持原有演示信任，业务流转不变。
			if tok == "" {
				writeErr(w, http.StatusUnauthorized, "wecom context required")
				return
			}
			if s.Context == nil {
				writeErr(w, http.StatusUnauthorized, "wecom context not enabled")
				return
			}
			p, tenant, cust, ok := s.Context.Verify(tok)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "invalid wecom context")
				return
			}
			if s.RoleResolver == nil {
				writeErr(w, http.StatusUnauthorized, "strict mode requires principal whitelist")
				return
			}
			role, _, ok := s.RoleResolver(p)
			if !ok {
				log.Printf("AUTH unknown principal=%q path=%s", p, r.URL.Path)
				writeErr(w, http.StatusUnauthorized, "unknown principal")
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserKey{}, ctxUser{UserID: p, Role: string(role), TenantID: tenant, CustomerID: cust})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			userID = "u-op"
		}
		customerID := ""
		// S1 上下文令牌（07 §6.1：服务端签发、不信任前端传入身份）：
		// 若携带 X-WeCom-Context，必须以令牌 principal 为准（ROL-01 收敛）。
		if tok != "" {
			if s.Context == nil {
				writeErr(w, http.StatusUnauthorized, "wecom context not enabled")
				return
			}
			p, tenant, cust, ok := s.Context.Verify(tok)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "invalid wecom context")
				return
			}
			userID = p // 身份以签名令牌为准，忽略请求头
			customerID = cust
			if s.RoleResolver == nil {
				_ = tenant
			}
		}
		var role authz.Role
		var tenant string
		if s.RoleResolver != nil {
			var ok bool
			role, tenant, ok = s.RoleResolver(userID)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "unknown principal")
				return
			}
		} else {
			role, tenant = s.RoleOf(userID)
		}
		ctx := context.WithValue(r.Context(), ctxUserKey{}, ctxUser{UserID: userID, Role: string(role), TenantID: tenant, CustomerID: customerID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isPublicPath 无需身份令牌即可访问的公开路径（UI 入口 + OAuth 引导端点）。
// 业务数据端点不在此列，一律走 X-WeCom-Context + 白名单强校验。
// 注意：/api/v1/bridge/auth-url 与 /api/v1/bridge/callback 故意不在豁免列表——
// strict 下 bridge 令牌按企微 principal 键存，这两个端点必须走 withContext 拿到身份。
func isPublicPath(p string) bool {
	switch {
	case p == "/healthz":
		return true
	case p == "/assistant" || strings.HasPrefix(p, "/assistant/"):
		return true
	case p == "/ops" || strings.HasPrefix(p, "/ops/"):
		return true
	case p == "/bridge" || strings.HasPrefix(p, "/bridge/"):
		return true
	case p == "/api/v1/wecom/jsapi-config":
		return true
	case p == "/api/v1/wecom/oauth-url":
		return true
	case p == "/api/v1/auth/exchange":
		return true
	}
	return false
}

type ctxUserKey struct{}

func userOf(ctx context.Context) ctxUser {
	u, _ := ctx.Value(ctxUserKey{}).(ctxUser)
	if u.TenantID == "" {
		u.TenantID = "chang-an"
	}
	return u
}

// ── 响应工具 ──

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// errStatus 错误 → HTTP 状态映射。
func errStatus(err error) int {
	var nf *repo.ErrNotFound
	if errors.As(err, &nf) {
		return http.StatusNotFound
	}
	if repo.IsIdempotencyConflict(err) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// ── 处理函数 ──

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// CreateOrderRequest 快速下单请求。
type CreateOrderRequest struct {
	CustomerID      string               `json:"customer_id"`
	PaymentTerm     string               `json:"payment_term"`
	SupplierChannel string               `json:"supplier_channel"`
	IdempotencyKey  string               `json:"idempotency_key"`
	Items           []CreateOrderItemReq `json:"items"`
}

// CreateOrderItemReq 下单明细。
type CreateOrderItemReq struct {
	ID             string `json:"id"`
	ProductID      string `json:"product_id"`
	WarehouseID    string `json:"warehouse_id"`
	Quantity       int64  `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	// ReceiveAddress 收货地址（A1：商务备货按收货地址选仓；可空）
	ReceiveAddress string `json:"receive_address"`
}

// handleCreateOrder 快速下单：角色闸门 → 幂等建单 → 幂等启动 Workflow。
func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	if !authz.CanCreateOrder(authz.Role(u.Role)) {
		writeErr(w, http.StatusForbidden, "role not allowed to create order")
		return
	}
	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.IdempotencyKey == "" || req.CustomerID == "" {
		writeErr(w, http.StatusBadRequest, "idempotency_key and customer_id are required")
		return
	}
	payload, _ := json.Marshal(req)
	items := make([]repo.NewOrderItem, 0, len(req.Items))
	for _, it := range req.Items {
		var wh *string
		if it.WarehouseID != "" {
			wh = &it.WarehouseID
		}
		items = append(items, repo.NewOrderItem{
			ID: it.ID, ProductID: it.ProductID, WarehouseID: wh,
			Quantity: it.Quantity, UnitPriceCents: it.UnitPriceCents,
			ReceiveAddress: it.ReceiveAddress,
		})
	}
	ord, created, err := s.Store.CreateOrder(r.Context(), repo.NewOrder{
		TenantID: u.TenantID, CustomerID: req.CustomerID, PaymentTerm: req.PaymentTerm,
		SupplierChannel: req.SupplierChannel, IdempotencyKey: req.IdempotencyKey, Payload: payload, Items: items,
	})
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if created {
		if _, err := s.Starter.StartOrderWorkflow(r.Context(), u.TenantID, ord.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "start workflow: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id": ord.ID, "state": ord.State, "created": created,
	})
}

// invoiceNoStore INV-01：发票号重复登记检查契约（repo.Repo 实现）。
type invoiceNoStore interface {
	InvoiceNoExists(ctx context.Context, tenantID, invoiceNo string) (bool, error)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// orderLister 全量订单列表仓储契约（repo.Repo 实现；SW2 商务全量视图 API-01 收敛）。
type orderLister interface {
	ListOrders(ctx context.Context, tenantID, state string, limit int) ([]repo.Order, error)
}

// orderListItem 列表投影（最小字段集；详情仍走 GET /orders/{id}）。
type orderListItem struct {
	ID              string  `json:"id"`
	State           string  `json:"state"`
	CustomerID      string  `json:"customer_id"`
	PaymentTerm     string  `json:"payment_term"`
	SupplierChannel string  `json:"supplier_channel"`
	ContractNo      *string `json:"contract_no,omitempty"`
	LegacyFlowID    *string `json:"legacy_flow_id,omitempty"`
	SubmissionMode  string  `json:"submission_mode"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// shipmentLister UI-02：订单发货单列表契约（repo.Repo 实现）。
type shipmentLister interface {
	ListShipmentsByOrder(ctx context.Context, tenantID, orderID string) ([]repo.Shipment, error)
}

// handleListOrderShipments 订单发货单列表（商务操作台凭证登记入口；OP/ADMIN）。
func (s *Server) handleListOrderShipments(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	if u.Role != string(authz.RoleOP) && u.Role != string(authz.RoleAdmin) {
		writeErr(w, http.StatusForbidden, "shipments list is ops-only")
		return
	}
	orderID := r.PathValue("orderID")
	if _, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	ls, ok := s.Store.(shipmentLister)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "shipment list store not supported")
		return
	}
	items, err := ls.ListShipmentsByOrder(r.Context(), u.TenantID, orderID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if items == nil {
		items = []repo.Shipment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shipments": items})
}

// handleListOrders SW2 商务全量订单/状态筛选（v0.1：商务 ops 只读视图；角色闸门 OP/ADMIN）。
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	if u.Role != string(authz.RoleOP) && u.Role != string(authz.RoleAdmin) {
		writeErr(w, http.StatusForbidden, "orders list is ops-only")
		return
	}
	ls, ok := s.Store.(orderLister)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "order list store not supported")
		return
	}
	limit := 100
	if n := r.URL.Query().Get("limit"); n != "" {
		if _, err := fmt.Sscan(n, &limit); err != nil || limit <= 0 || limit > 500 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
	}
	orders, err := ls.ListOrders(r.Context(), u.TenantID, r.URL.Query().Get("state"), limit)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if orders == nil {
		orders = []repo.Order{}
	}
	items := make([]orderListItem, 0, len(orders))
	for _, o := range orders {
		items = append(items, orderListItem{
			ID: o.ID, State: o.State, CustomerID: o.CustomerID, PaymentTerm: o.PaymentTerm,
			SupplierChannel: o.SupplierChannel, ContractNo: o.ContractNo, LegacyFlowID: o.LegacyFlowID,
			SubmissionMode: o.SubmissionMode,
			CreatedAt:      o.CreatedAt.Format(time.RFC3339), UpdatedAt: o.UpdatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": items, "count": len(items)})
}

// handleOrderSignal 审批动作（Signal 分发；人工闸门不可绕过；AUTH-01 信号级角色闸门）。
func (s *Server) handleOrderSignal(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	signal := r.PathValue("signal")
	ord, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	payload, ok := signalPayload(signal, r.Body)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown signal or invalid payload: "+signal)
		return
	}
	if !authz.CanSignal(authz.Role(u.Role), signal) {
		writeErr(w, http.StatusForbidden, "role not allowed for signal "+signal)
		return
	}
	// 事件链可信：Operator 一律取鉴权后的当前用户，禁止请求体伪造审批人
	payload = stampSignalOperator(payload, u.UserID)
	workflowID := WorkflowID(u.TenantID, ord.IdempotencyKey)
	if err := s.Starter.SignalWorkflow(r.Context(), workflowID, signal, payload); err != nil {
		writeErr(w, http.StatusInternalServerError, "signal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "workflow_id": workflowID})
}

// handleGetOrder 订单查询（BFF 字段投影：仓管剥离价格）。
func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	ord, err := s.Store.GetOrder(r.Context(), u.TenantID, r.PathValue("orderID"))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	items, err := s.Store.GetOrderItems(r.Context(), u.TenantID, ord.ID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	// 授权预检（独立验证者；视图级）
	ok, err := s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermView, ResourceType: authz.ResOrder, ResourceID: ord.ID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	var views []OrderItemView
	unitPrice := int64(0)
	if len(items) > 0 {
		unitPrice = items[0].UnitPriceCents
	}
	for _, it := range items {
		views = append(views, OrderItemView{ProductID: it.ProductID, Quantity: it.Quantity, ReceiveAddress: it.ReceiveAddress})
	}
	amount := unitPrice * sumQty(items)
	view := ProjectOrder(ord.ID, ord.State, ord.PaymentTerm, ord.SupplierChannel, ord.ContractNo,
		views, unitPrice, amount, authz.Role(u.Role))
	writeJSON(w, http.StatusOK, view)
}

// handleOrderEvents 事件链回放（CQ-12）。
func (s *Server) handleOrderEvents(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	if _, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	events, err := s.Store.ListOrderEvents(r.Context(), u.TenantID, orderID, 500)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleTodos 待办检索（CQ-05）。
func (s *Server) handleTodos(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	assignee := r.URL.Query().Get("assignee")
	kind := r.URL.Query().Get("kind")
	state := r.URL.Query().Get("state")
	tasks, err := s.Store.ListApprovalTasks(r.Context(), u.TenantID, assignee, kind, state, 100)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleAppendEvidence 追加凭证版本（CQ-15 场景 D：uploader 权限）。
func (s *Server) handleAppendEvidence(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	shipmentID := r.PathValue("shipmentID")
	shp, err := s.Store.GetShipment(r.Context(), u.TenantID, shipmentID)
	if err != nil || shp.OrderID != orderID {
		writeErr(w, http.StatusNotFound, "shipment not found")
		return
	}
	ok, err := s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermAppendEvidence, ResourceType: authz.ResShipment,
		ResourceID: shipmentID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	var in struct {
		FileURL  string `json:"file_url"`
		FileHash string `json:"file_hash"`
		// A2/CW1 识别登记字段（凭证机器人 OCR 与后台上传共用）：kind/source/发票号/金额/税额
		Kind        string `json:"kind"`
		Source      string `json:"source"`
		InvoiceNo   string `json:"invoice_no"`
		AmountCents int64  `json:"amount_cents"`
		TaxCents    int64  `json:"tax_cents"`
		// BuyerTaxNo INV-01：购方税号（发票抬头校验，可选；配置 pgAPI 时与客户档案比对）
		BuyerTaxNo string `json:"buyer_tax_no"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.FileURL == "" || in.FileHash == "" {
		writeErr(w, http.StatusBadRequest, "file_url and file_hash required")
		return
	}
	// INV-01：开票凭证四项硬校验（订单唯一=shipment↔order 已校验；发票不重复；金额守恒；税号一致）
	if in.Kind == "INVOICE" {
		if in.InvoiceNo == "" {
			writeErr(w, http.StatusBadRequest, "invoice_no required for INVOICE evidence")
			return
		}
		if ins, okStore := s.Store.(invoiceNoStore); okStore {
			exists, err := ins.InvoiceNoExists(r.Context(), u.TenantID, in.InvoiceNo)
			if err != nil {
				writeErr(w, errStatus(err), err.Error())
				return
			}
			if exists {
				writeErr(w, http.StatusConflict, "invoice_no already registered (非重复发票)")
				return
			}
		}
		if in.BuyerTaxNo != "" && s.Data != nil {
			ord, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID)
			if err != nil {
				writeErr(w, errStatus(err), err.Error())
				return
			}
			cust, err := s.Data.GetCustomer(r.Context(), u.TenantID, ord.CustomerID)
			if err != nil {
				writeErr(w, errStatus(err), err.Error())
				return
			}
			want := firstNonEmptyStr(cust.InvoiceTaxNo, cust.TaxNo)
			if want != "" && want != in.BuyerTaxNo {
				writeErr(w, http.StatusBadRequest, "buyer_tax_no 与客户档案不一致（抬头税号校验）")
				return
			}
		}
	}
	// N-06 凭证金额守恒：开票金额 = 已发货金额（不超发货）；超限拒绝（409）
	if in.Kind == "INVOICE" && in.AmountCents > 0 {
		sc, okStore := s.Store.(shipmentAmountStore)
		if !okStore {
			writeErr(w, http.StatusNotImplemented, "shipment amount store not supported")
			return
		}
		shipped, err := sc.ShipmentShippedAmountCents(r.Context(), u.TenantID, shipmentID)
		if err != nil {
			writeErr(w, errStatus(err), err.Error())
			return
		}
		if in.AmountCents > shipped {
			writeErr(w, http.StatusConflict, "开票金额超出已发货金额（守恒校验）")
			return
		}
	}
	ems, okStore := s.Store.(evidenceMetaStore)
	if !okStore {
		writeErr(w, http.StatusNotImplemented, "evidence meta store not supported")
		return
	}
	ver, err := ems.AppendEvidenceMeta(r.Context(), u.TenantID, shipmentID, u.UserID, in.FileURL, in.FileHash, repo.EvidenceMeta{
		Kind: in.Kind, Source: in.Source, InvoiceNo: in.InvoiceNo, AmountCents: in.AmountCents, TaxCents: in.TaxCents,
	})
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"shipment_id": shipmentID, "version": ver})
}

// handleListEvidence 回放凭证版本（CQ-15：任一版本可回放；view 权限）。
func (s *Server) handleListEvidence(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	shipmentID := r.PathValue("shipmentID")
	shp, err := s.Store.GetShipment(r.Context(), u.TenantID, shipmentID)
	if err != nil || shp.OrderID != orderID {
		writeErr(w, http.StatusNotFound, "shipment not found")
		return
	}
	ok, err := s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermView, ResourceType: authz.ResShipment,
		ResourceID: shipmentID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	vs, err := s.Store.ListEvidenceVersions(r.Context(), u.TenantID, shipmentID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shipment_id": shipmentID, "versions": vs})
}

// CreatePreOrderRequest 预订单创建请求（企微消息入口；消息幂等）。
type CreatePreOrderRequest struct {
	GroupID       string          `json:"group_id"`
	MsgID         string          `json:"msg_id"`
	IntentVersion string          `json:"intent_version"`
	Candidates    json.RawMessage `json:"candidates"`
	SenderID      string          `json:"sender_id"` // 07 §3.2 权限校验输入
}

// handleCreatePreOrder 消息 → 预订单（msgid 幂等）→ 启动 PreOrderWorkflow。
func (s *Server) handleCreatePreOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	var req CreatePreOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	key, err := keyFor("pre", u.TenantID, req.GroupID, req.MsgID, req.IntentVersion)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Candidates) == 0 {
		req.Candidates = json.RawMessage(`[]`)
	}
	po, created, err := s.Store.CreatePreOrder(r.Context(), repo.NewPreOrder{
		TenantID: u.TenantID, GroupID: req.GroupID, MsgID: req.MsgID,
		Candidates: req.Candidates, IdempotencyKey: key,
	})
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if created {
		if _, err := s.Starter.StartPreOrderWorkflow(r.Context(), u.TenantID, po.ID, req.SenderID); err != nil {
			writeErr(w, http.StatusInternalServerError, "start workflow: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pre_order_id": po.ID, "state": po.State, "created": created,
	})
}

// handlePreOrderSignal 预订单动作（确认/忽略）。
func (s *Server) handlePreOrderSignal(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	po, err := s.Store.GetPreOrder(r.Context(), u.TenantID, r.PathValue("preOrderID"))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	signal := r.PathValue("signal")
	payload, ok := preOrderSignalPayload(signal, r.Body)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown preorder signal: "+signal)
		return
	}
	// N-02（A3/A7）：销售是智能下单唯一可确认角色；非销售 → 403
	if u.Role != string(authz.RoleSales) && u.Role != string(authz.RoleAdmin) {
		writeErr(w, http.StatusForbidden, "preorder confirm is sales-only")
		return
	}
	payload = stampSignalOperator(payload, u.UserID)
	// 确认只发生一次：终态预订单再确认 → 409 幂等回执（不再投递信号）
	switch po.State {
	case domain.StatePreConfirmed, domain.StatePreModified, domain.StatePreOrderRequested, domain.StatePreIgnored:
		writeErr(w, http.StatusConflict, "preorder already decided")
		return
	}
	workflowID := "preorder-" + u.TenantID + "-" + po.ID
	if err := s.Starter.SignalWorkflow(r.Context(), workflowID, signal, payload); err != nil {
		writeErr(w, http.StatusInternalServerError, "signal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "workflow_id": workflowID})
}

// handleGetPreOrder 预订单查询。
func (s *Server) handleGetPreOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	po, err := s.Store.GetPreOrder(r.Context(), u.TenantID, r.PathValue("preOrderID"))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, po)
}

// ── Signal 载荷解析 ──

// WorkflowID 订单工作流 ID（02 §5 层 2：tenant 维度天然隔离）。
func WorkflowID(tenantID, idempotencyKey string) string {
	return fmt.Sprintf("order-%s-%s", tenantID, idempotencyKey)
}
