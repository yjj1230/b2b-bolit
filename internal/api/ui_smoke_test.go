package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/workflows"
)

// UI smoke（静态层）：两个生产页面必须内嵌并包含关键控件与全部 API 调用面。
func TestEmbeddedUIsSmoke(t *testing.T) {
	store := workflows.NewFakeStoreForTest()
	srv := NewServer(store, newFakeStarter(store), nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	h := srv.Routes()

	rec := doJSON(t, h, http.MethodGet, "/ops", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	ops := rec.Body.String()
	for _, needle := range []string{
		"商务最小操作台", "loadTodos", "loadOrders", "loadShipments",
		"/api/v1/todos", "/api/v1/orders", "/api/v1/orders/", "/shipments/", "/evidence",
		"/signal/",
	} {
		require.True(t, strings.Contains(ops, needle), "ops page missing %s", needle)
	}

	rec = doJSON(t, h, http.MethodGet, "/assistant", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	asst := rec.Body.String()
	for _, needle := range []string{
		"订单助手 · 销售助理", "loadPrefill", "customer-list", "product-list",
		"/api/v1/orderdata/customers", "/api/v1/orderdata/products",
		"/api/v1/bridge/orders", "/api/v1/auth/exchange",
		"网页工作台", "人工下单（代下单）", "代下单归属（先选）", "下单归属销售",
		"订单信息", "商品明细", "收货方式", "结算方式", "customer_type", "btn-add-item",
		"产品型号", "打包备注", "是否需要质检报告", "packing_remark", "quality_report_required",
		"value=\"CNY\"", "value=\"USD\"", "value=\"EUR\"", "currency: $('currency').value",
		"长安仁恒包装", "克莱明顿包装", "小袋套吨袋", "打托(木托盘)", "请选择规格",
		"order-quantity-total", "order-total", "提交订单", "搜索、选择或填写收货人",
		"sales-display-list", "supplier-list", "contact-list", "address-list", "step=\"0.0001\"", "unit_price:",
		"人民币 (元)", "美元 (USD)", "欧元 (EUR)", "item-product-clear",
	} {
		require.True(t, strings.Contains(asst, needle), "assistant page missing %s", needle)
	}
	for _, forbidden := range []string{
		"contract_file", "customer_contract_no", "contract-list", "/api/v1/orderdata/purchase-contracts",
		"上传客户合同", "框架合同", "other_files",
	} {
		require.NotContains(t, asst, forbidden, "assistant page must hide contract/placeholder capability: %s", forbidden)
	}
}
