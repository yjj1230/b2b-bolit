package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

// AUTH-01 收敛：信号级角色闸门 + Operator 以鉴权身份覆盖（防伪造审批人）。
func TestOrderSignalRoleGateAndOperator(t *testing.T) {
	srv, starter, store := newAPITest(t)
	seedAPIWorld(t, store)
	h := srv.Routes()
	orderID := createAPIOrder(t, h) // 建立真实订单（ORD_REQUESTED，GetOrder 可用）

	// OP 无权投递 price-review（审价是主管专属）
	rec := doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/signal/price-review",
		`{"verdict":"APPROVED","operator":"fake-chairman"}`, map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Len(t, starter.signals, 0)

	// 仓库角色不能投递备货信号
	rec = doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/signal/stock-review",
		`{"verdict":"APPROVED","warehouse_id":"wh-yangyuan","operator":"fake"}`, map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 商务 OP 投递备货信号：放行，且 Operator 被覆盖为鉴权用户 u-op
	rec = doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/signal/stock-review",
		`{"verdict":"APPROVED","warehouse_id":"wh-yangyuan","operator":"fake-chairman"}`, map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, starter.signals, 1)
	sig, ok := starter.signals[0].Payload.(workflows.StockReviewSignal)
	require.True(t, ok)
	require.Equal(t, "APPROVED", sig.Verdict)
	require.Equal(t, "u-op", sig.Operator, "operator 必须来自鉴权身份而非请求体")

	// 未知信号：解析层拒绝（先于角色矩阵，返回 400）
	rec = doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/signal/no-such-signal", `{}`, map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// AUTH-01 收敛：创建订单角色闸门（仓管/财务不可建单）。
func TestCreateOrderRoleGate(t *testing.T) {
	srv, _, _ := newAPITest(t)
	h := srv.Routes()
	body := `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:gate-1",
		"items":[{"id":"item-1","product_id":"prod-c68","quantity":1,"unit_price_cents":100}]}`
	rec := doJSON(t, h, http.MethodPost, "/api/v1/orders", body, map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doJSON(t, h, http.MethodPost, "/api/v1/orders", body, map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ROL-01 收敛：生产白名单身份解析（未命中 → 401，不再默认当商务）。
func TestRoleResolverWhitelist(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.RoleResolver = func(userID string) (authz.Role, string, bool) {
		if userID == "u-op" {
			return authz.RoleOP, "chang-an", true
		}
		if userID == "u-accounting" {
			return authz.RoleAccounting, "chang-an", true
		}
		return "", "", false
	}
	h := srv.Routes()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/todos", "", map[string]string{"X-User-Id": "attacker"})
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	rec = doJSON(t, h, http.MethodGet, "/api/v1/todos", "", map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// API-01 收敛：商务全量订单列表端点（角色闸门 + 状态筛选 + 列表投影）。
type listOrdersStore struct {
	*workflows.FakeStore
	orders []repo.Order
}

func (s *listOrdersStore) ListOrders(ctx context.Context, tenantID, state string, limit int) ([]repo.Order, error) {
	return s.orders, nil
}

func TestListOrdersHTTP(t *testing.T) {
	base := workflows.NewFakeStoreForTest()
	ls := &listOrdersStore{FakeStore: base, orders: []repo.Order{
		{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", State: "ORD_PRICE_REVIEWING",
			PaymentTerm: "款到", SupplierChannel: "渠道A", SubmissionMode: "manual"},
		{ID: "ORD-2", TenantID: "chang-an", CustomerID: "cust-0002", State: "INV_CLOSED",
			PaymentTerm: "非款到", SupplierChannel: "渠道B", SubmissionMode: "sync"},
	}}
	starter := newFakeStarter(ls)
	checker := authz.NewStaticChecker(func(string) (authz.Role, error) { return authz.RoleOP, nil })
	srv := NewServer(ls, starter, checker, func(userID string) (authz.Role, string) {
		if userID == "frank-wh" {
			return authz.RoleWarehouse, "chang-an"
		}
		return authz.RoleOP, "chang-an"
	})
	h := srv.Routes()

	// 非商务 403
	rec := doJSON(t, h, http.MethodGet, "/api/v1/orders", "", map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 商务全量
	rec = doJSON(t, h, http.MethodGet, "/api/v1/orders", "", map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "ORD-1")
	require.Contains(t, rec.Body.String(), "ORD-2")
	require.Contains(t, rec.Body.String(), `"count":2`)

	// 非法 limit
	rec = doJSON(t, h, http.MethodGet, "/api/v1/orders?limit=abc", "", map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
