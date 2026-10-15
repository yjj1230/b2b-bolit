package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

// UI-01 收敛：pgAPI 搜索端点（客户/产品/合同预填）+ 未配置/角色拒绝降级路径。
func TestOrderdataSearchEndpoints(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	seedAPIWorld(t, store)
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Data = orderdata.NewDemoMemoryProvider()
	h := srv.Routes()

	rec := doJSON(t, h, http.MethodGet, "/api/v1/orderdata/customers?q=华信", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "cust-0001")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/orderdata/products", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "标准电机 C68")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/orderdata/purchase-contracts", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "HT-chang-an-demo")

	// 未配置 pgAPI → 501（页面降级手工填写）
	noData := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	rec = doJSON(t, noData.Routes(), http.MethodGet, "/api/v1/orderdata/customers", "", nil)
	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())

	// 仓管不可用 → 403
	roleSrv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleWarehouse, "chang-an" })
	roleSrv.Data = orderdata.NewDemoMemoryProvider()
	rec = doJSON(t, roleSrv.Routes(), http.MethodGet, "/api/v1/orderdata/customers", "", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// UI-02 收敛：订单发货单列表端点与商务操作台页面。
func TestOpsUIRoutesAndShipmentsList(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	seedAPIWorld(t, store)
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	h := srv.Routes()

	rec := doJSON(t, h, http.MethodGet, "/ops", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	require.Contains(t, rec.Body.String(), "商务最小操作台")

	// 未配置 ListShipmentsByOrder 的 FakeStore → 501；OP 可达
	orderID := createAPIOrder(t, h)
	rec = doJSON(t, h, http.MethodGet, "/api/v1/orders/"+orderID+"/shipments", "", nil)
	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
}

type searchErrProvider struct{ *orderdata.MemoryProvider }

func (p *searchErrProvider) SearchCustomers(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Customer], error) {
	return nil, errors.New("pgapi boom")
}
func (p *searchErrProvider) SearchProducts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Product], error) {
	return nil, errors.New("pgapi boom")
}
func (p *searchErrProvider) SearchPurchaseContracts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.PurchaseContract], error) {
	return nil, errors.New("pgapi boom")
}

type shipListErrStore struct{ *workflows.FakeStore }

func (s *shipListErrStore) ListShipmentsByOrder(ctx context.Context, tenantID, orderID string) ([]repo.Shipment, error) {
	return nil, errors.New("db boom")
}

// UI-01/UI-02 分支补强：查询参数、pgAPI 错误、发货单列表角色/错误路径。
func TestOrderdataAndShipmentBranches(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	seedAPIWorld(t, store)
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Data = orderdata.NewDemoMemoryProvider()
	h := srv.Routes()

	// dataQuery 参数分支
	for _, qs := range []string{"?page_size=10", "?page_size=999", "?grade=C&product_name=x&customer_name=y&customer_id=c1"} {
		rec := doJSON(t, h, http.MethodGet, "/api/v1/orderdata/customers"+qs, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// pgAPI 错误分支（三个端点）
	srvErr := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srvErr.Data = &searchErrProvider{MemoryProvider: orderdata.NewMemoryProvider()}
	he := srvErr.Routes()
	for _, path := range []string{"/api/v1/orderdata/customers", "/api/v1/orderdata/products", "/api/v1/orderdata/purchase-contracts"} {
		rec := doJSON(t, he, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
	}

	// 发货单列表：仓管 403；store 错误 400
	orderID := createAPIOrder(t, h)
	roleSrv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleWarehouse, "chang-an" })
	rec := doJSON(t, roleSrv.Routes(), http.MethodGet, "/api/v1/orders/"+orderID+"/shipments", "", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	errStore := &shipListErrStore{FakeStore: store}
	srvShip := NewServer(errStore, newFakeStarter(errStore), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	rec = doJSON(t, srvShip.Routes(), http.MethodGet, "/api/v1/orders/"+orderID+"/shipments", "", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 订单不存在 → 404
	rec = doJSON(t, srvShip.Routes(), http.MethodGet, "/api/v1/orders/ORD-nope/shipments", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

type nilSearchProvider struct{ *orderdata.MemoryProvider }

func (p *nilSearchProvider) SearchProducts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Product], error) {
	return nil, nil
}
func (p *nilSearchProvider) SearchPurchaseContracts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.PurchaseContract], error) {
	return nil, nil
}

// nil 结果归一化分支（保证列表端点永远返回数组而非 null）。
func TestSearchNilResultNormalized(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Data = &nilSearchProvider{MemoryProvider: orderdata.NewMemoryProvider()}
	h := srv.Routes()
	for _, path := range []string{"/api/v1/orderdata/products", "/api/v1/orderdata/purchase-contracts"} {
		rec := doJSON(t, h, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Contains(t, rec.Body.String(), `"items":[]`)
	}
}

// UI-01 收敛：老客户搜索按销售员姓名过滤（pgAPI salesperson 参数的内存等价验证）。
func TestSearchCustomersSalespersonFilter(t *testing.T) {
	m := orderdata.NewMemoryProvider()
	seed := []pgapi.CustomerWrite{
		{CustomerName: "北京华信科技", CustomerLevel: "A", Supplier: "渠道A", SalesUsers: []pgapi.SalesUser{{ID: "u-a", Name: "张有连"}}},
		{CustomerName: "上海远景制造", CustomerLevel: "C", Supplier: "渠道B", SalesUsers: []pgapi.SalesUser{{ID: "u-b", Name: "李四"}}},
		{CustomerName: "无主客户", CustomerLevel: "A", Supplier: "渠道A"},
	}
	for _, w := range seed {
		if _, err := m.CreateCustomer(context.Background(), "chang-an", w); err != nil {
			t.Fatal(err)
		}
	}
	store := workflows.NewFakeStoreForTest()
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Data = m
	h := srv.Routes()

	// 按销售员姓名过滤：只出张有连名下客户
	rec := doJSON(t, h, http.MethodGet, "/api/v1/orderdata/customers?salesperson="+url.QueryEscape("张有连"), "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "北京华信科技")
	require.NotContains(t, rec.Body.String(), "上海远景制造")
	require.NotContains(t, rec.Body.String(), "无主客户")

	// 未匹配到销售员 → 空
	rec = doJSON(t, h, http.MethodGet, "/api/v1/orderdata/customers?salesperson="+url.QueryEscape("不存在的人"), "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"items":[]`)

	// 不传 salesperson → 全量（未选销售员由前端拦截，服务端保持兼容）
	rec = doJSON(t, h, http.MethodGet, "/api/v1/orderdata/customers", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "北京华信科技")
	require.Contains(t, rec.Body.String(), "上海远景制造")
	require.Contains(t, rec.Body.String(), "无主客户")
}
