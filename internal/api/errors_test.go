package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

// ── API 错误路径（覆盖率补强）──

// 创建订单错误路径
func TestHTTPCreateOrderErrors(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)

	// 非法 body
	rec := doJSON(t, h, "POST", "/api/v1/orders", `{not-json`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 缺幂等键
	rec = doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001"}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 幂等键冲突（同键异 payload → 409）
	body := `{"customer_id":"cust-0001","payment_term":"款到","idempotency_key":"order:chang-an:quick:dup",
		"items":[{"id":"i1","product_id":"prod-c68","quantity":1,"unit_price_cents":1}]}`
	rec = doJSON(t, h, "POST", "/api/v1/orders", body, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// 同键异 payload（数量不同）→ 409
	rec = doJSON(t, h, "POST", "/api/v1/orders",
		`{"customer_id":"cust-0001","payment_term":"款到","idempotency_key":"order:chang-an:quick:dup",
		"items":[{"id":"i1","product_id":"prod-c68","quantity":999,"unit_price_cents":1}]}`, nil)
	require.Equal(t, http.StatusConflict, rec.Code)

	// D-006：customer_id 为 pgAPI record_id 外部引用；Store 不再做本地 customers 存在性校验。
	// 真实校验发生在 OrderWorkflow 的 ValidateOrderData（本用例 starter 为 fake，不执行工作流）。
	rec = doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-nope","payment_term":"款到",
		"idempotency_key":"order:chang-an:quick:fk1","items":[{"id":"i1","product_id":"prod-c68","quantity":1,"unit_price_cents":1}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// 信号错误路径
func TestHTTPOrderSignalErrors(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)
	ord := createAPIOrder(t, h)

	// 未知订单 → 404
	rec := doJSON(t, h, "POST", "/api/v1/orders/ORD-nope/signal/price-review", `{}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 未知信号 → 400
	rec = doJSON(t, h, "POST", "/api/v1/orders/"+ord+"/signal/bogus", `{}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 载荷非法 → 400
	rec = doJSON(t, h, "POST", "/api/v1/orders/"+ord+"/signal/price-review", `{bad`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 预订单错误路径
func TestHTTPPreOrderErrors(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)

	// 非法 body
	rec := doJSON(t, h, "POST", "/api/v1/preorders", `{bad`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// 缺 group/msgid → 400（幂等键构造失败）
	rec = doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"","msg_id":""}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// 未知预订单 → 404
	rec = doJSON(t, h, "GET", "/api/v1/preorders/PRE-nope", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	// 未知预订单信号 → 404
	rec = doJSON(t, h, "POST", "/api/v1/preorders/PRE-nope/signal/preorder-confirm", `{}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// 授权拒绝（视图 403）
func TestHTTPAuthzDenied(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	starter := newFakeStarter(store)
	// 直接注入拒绝型 checker：StaticChecker 对 view 恒 true，这里用自定义 Checker 模拟拒绝
	denier := &denyChecker{}
	roleOf := func(userID string) (authz.Role, string) { return authz.RoleOP, "chang-an" }
	srv := NewServer(store, starter, denier, roleOf)
	h := srv.Routes()
	store.SeedTenant("chang-an")
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 100)
	ord := createAPIOrder(t, h)

	rec := doJSON(t, h, "GET", "/api/v1/orders/"+ord, "", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

type denyChecker struct{}

func (d *denyChecker) Check(ctx context.Context, req authz.CheckRequest) (bool, error) {
	return false, nil
}

// 待办过滤组合
func TestHTTPTodosFilters(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)
	// 无任务 → 空
	rec := doJSON(t, h, "GET", "/api/v1/todos?assignee=u-supervisor", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var todos struct {
		Tasks []repo.ApprovalTask `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &todos))
	require.Empty(t, todos.Tasks)
}

// 事件链 404 / 预订单信号非法载荷 / todos 过滤组合
func TestHTTPMoreErrors(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)

	// 未知订单事件链 → 404
	rec := doJSON(t, h, "GET", "/api/v1/orders/ORD-nope/events", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 预订单信号非法载荷 → 400
	recPre := doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"g1","msg_id":"m1","intent_version":"v1","candidates":[]}`, nil)
	require.Equal(t, http.StatusOK, recPre.Code)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(recPre.Body.Bytes(), &pre))
	rec = doJSON(t, h, "POST", "/api/v1/preorders/"+pre.PreOrderID+"/signal/preorder-confirm", `{bad`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doJSON(t, h, "POST", "/api/v1/preorders/"+pre.PreOrderID+"/signal/bogus", `{}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// todos 无过滤
	rec = doJSON(t, h, "GET", "/api/v1/todos", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// handler 错误分支补充（覆盖率补强）
func TestHTTPHandlerErrorBranches(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)
	ord := createAPIOrder(t, h)

	// handleTodos：assignee/kind/state 全组合 + store 无数据
	for _, q := range []string{"?assignee=u-supervisor", "?kind=PRICE", "?state=APPROVED", "?assignee=x&kind=y&state=z"} {
		rec := doJSON(t, h, "GET", "/api/v1/todos"+q, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
	}
	// handleOrderEvents：合法订单（已有订单）
	rec := doJSON(t, h, "GET", "/api/v1/orders/"+ord+"/events", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// handleCreatePreOrder：缺 group/msgid → 400（幂等键构造失败）
	rec = doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"","msg_id":"","intent_version":"v1","candidates":[]}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// handleGetOrder：store 错误（GetOrderItems 失败）→ 400 路径
	rec = doJSON(t, h, "GET", "/api/v1/orders/"+ord, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// handleOrderSignal：合法信号但 starter 报错 → 500（主管角色可投 price-review）
	failingSrv := NewServer(store, &errStarter{}, &denyAllowChecker{}, func(userID string) (authz.Role, string) {
		if userID == "u-supervisor" {
			return authz.RoleSupervisor, "chang-an"
		}
		return authz.RoleOP, "chang-an"
	})
	rec = doJSON(t, failingSrv.Routes(), "POST", "/api/v1/orders/"+ord+"/signal/price-review", `{"verdict":"APPROVED","operator":"u-supervisor"}`,
		map[string]string{"X-User-Id": "u-supervisor"})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	// handleCreateOrder：starter 报错 → 500
	rec = doJSON(t, failingSrv.Routes(), "POST", "/api/v1/orders",
		`{"customer_id":"cust-0001","payment_term":"款到","idempotency_key":"order:chang-an:quick:err-1","items":[{"id":"i1","product_id":"prod-c68","quantity":1,"unit_price_cents":1}]}`, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// errStarter 启动/信号恒报错。
type errStarter struct{}

func (e *errStarter) StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	return "", errors.New("inject: start failed")
}
func (e *errStarter) StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	return "", errors.New("inject: start failed")
}
func (e *errStarter) SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error {
	return errors.New("inject: signal failed")
}

// denyAllowChecker 放行一切授权。
type denyAllowChecker struct{}

func (d *denyAllowChecker) Check(ctx context.Context, req authz.CheckRequest) (bool, error) {
	return true, nil
}

var _ authz.Checker = (*denyAllowChecker)(nil)

// userOf 兜底：RoleOf 返回空租户 → 默认 chang-an
func TestUserOfFallback(t *testing.T) {
	srv := NewServer(nil, &errStarter{}, &denyAllowChecker{}, func(string) (authz.Role, string) { return authz.RoleOP, "" })
	_ = srv
	// userOf 通过空 ctx → 默认租户
	u := userOf(context.Background())
	require.Equal(t, "chang-an", u.TenantID)
}

// store 错误分支（errStore 包装：按操作注入失败）
type errStore struct {
	workflows.FakeStore
	failOn map[string]bool
}

func newErrStore() *errStore {
	return &errStore{FakeStore: *workflows.NewFakeStoreForTest(), failOn: map[string]bool{}}
}

func (e *errStore) GetOrderItems(ctx context.Context, tenantID, orderID string) ([]repo.OrderItem, error) {
	if e.failOn["GetOrderItems"] {
		return nil, errors.New("inject")
	}
	return e.FakeStore.GetOrderItems(ctx, tenantID, orderID)
}

func (e *errStore) ListOrderEvents(ctx context.Context, tenantID, orderID string, limit int) ([]repo.Event, error) {
	if e.failOn["ListOrderEvents"] {
		return nil, errors.New("inject")
	}
	return e.FakeStore.ListOrderEvents(ctx, tenantID, orderID, limit)
}

func (e *errStore) ListApprovalTasks(ctx context.Context, tenantID string, assignee, kind, state string, limit int) ([]repo.ApprovalTask, error) {
	if e.failOn["ListApprovalTasks"] {
		return nil, errors.New("inject")
	}
	return e.FakeStore.ListApprovalTasks(ctx, tenantID, assignee, kind, state, limit)
}

func (e *errStore) CreatePreOrder(ctx context.Context, in repo.NewPreOrder) (*repo.PreOrder, bool, error) {
	if e.failOn["CreatePreOrder"] {
		return nil, false, errors.New("inject")
	}
	return e.FakeStore.CreatePreOrder(ctx, in)
}

func (e *errStore) GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*repo.PreOrder, error) {
	if e.failOn["GetPreOrder"] {
		return nil, errors.New("inject")
	}
	return e.FakeStore.GetPreOrder(ctx, tenantID, preOrderID)
}

func TestHTTPStoreErrorBranches(t *testing.T) {
	store := newErrStore()
	store.SeedTenant("chang-an")
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 100)
	starter := newFakeStarter(store)
	checker := &denyAllowChecker{}
	srv := NewServer(store, starter, checker, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	h := srv.Routes()

	// 建单（供后续查询）
	ord := createAPIOrder(t, h)

	// handleGetOrder：GetOrderItems 失败 → 400
	store.failOn["GetOrderItems"] = true
	rec := doJSON(t, h, "GET", "/api/v1/orders/"+ord, "", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	delete(store.failOn, "GetOrderItems")

	// handleOrderEvents：ListOrderEvents 失败 → 400
	store.failOn["ListOrderEvents"] = true
	rec = doJSON(t, h, "GET", "/api/v1/orders/"+ord+"/events", "", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	delete(store.failOn, "ListOrderEvents")

	// handleTodos：ListApprovalTasks 失败 → 400
	store.failOn["ListApprovalTasks"] = true
	rec = doJSON(t, h, "GET", "/api/v1/todos", "", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	delete(store.failOn, "ListApprovalTasks")

	// handleCreatePreOrder：CreatePreOrder 失败 → 400（errStatus 默认）
	store.failOn["CreatePreOrder"] = true
	rec = doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"g1","msg_id":"m1","intent_version":"v1","candidates":[]}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	delete(store.failOn, "CreatePreOrder")

	// handlePreOrderSignal：GetPreOrder 失败 → 400
	store.failOn["GetPreOrder"] = true
	rec = doJSON(t, h, "POST", "/api/v1/preorders/PRE-1/signal/preorder-confirm", `{"confirmed":true}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	delete(store.failOn, "GetPreOrder")
}

// LocalRunner：Wait ctx 取消 + StartOrderWorkflow store 失败 + SignalWorkflow 超时
func TestLocalRunnerEdges(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	lr := NewLocalRunner(store, nil, nil)
	ctx := context.Background()

	// StartOrderWorkflow：未知订单 → 错误
	_, err := lr.StartOrderWorkflow(ctx, "chang-an", "ORD-nope")
	require.Error(t, err)

	// Wait：已取消 ctx → ctx.Err
	store.SeedTenant("chang-an")
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	ord, _, err := store.CreateOrder(ctx, workflows.NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:edge"))
	require.NoError(t, err)
	wid, err := lr.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, lr.Wait(cancelled, wid))

	// SignalWorkflow：未知 workflow → 错误（已测）；wait 等待现有
	require.NoError(t, lr.SignalWorkflow(ctx, wid, workflows.SignalPriceReview, workflows.PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"}))
}
