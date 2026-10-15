package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── N-05 · 多单合并开票端点（A4）：link-orders 追加关联（幂等/守恒/参数/404）+ invoice-summary 三金额分展 ──

func TestN05InvoiceLinkAndSummaryHTTP(t *testing.T) {
	srv, store, _ := newV01Test(t)
	h := srv.Routes()

	// 订单 1：已开票（主关联）
	order1 := createAPIOrder(t, h)
	store.SeedPrepared("chang-an", order1, "item-1", 100)
	store.SeedShipped("chang-an", order1, "item-1", 100)
	store.SeedInvoice("chang-an", "INV-n05", order1, 100000, 100)

	// 订单 2：已发货未开票
	rec2 := doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到",
		"idempotency_key":"order:chang-an:n05:o2",
		"items":[{"id":"item-n05-2","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`, nil)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var created2 struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &created2))
	store.SeedPrepared("chang-an", created2.OrderID, "item-n05-2", 100)
	store.SeedShipped("chang-an", created2.OrderID, "item-n05-2", 100)

	linkBody := func(orderID string, amt, qty int64) string {
		return `{"order_id":"` + orderID + `","amount_cents":` + itoa64(amt) + `,"qty":` + itoa64(qty) + `}`
	}

	// 追加关联订单 2 → 200 created
	recL := doJSON(t, h, "POST", "/api/v1/invoices/INV-n05/link-orders", linkBody(created2.OrderID, 100000, 100), nil)
	require.Equal(t, http.StatusOK, recL.Code, recL.Body.String())
	var linkOut struct {
		OK      bool `json:"ok"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(recL.Body.Bytes(), &linkOut))
	require.True(t, linkOut.OK)
	require.True(t, linkOut.Created)

	// 重复追加 → 幂等 created=false
	recL2 := doJSON(t, h, "POST", "/api/v1/invoices/INV-n05/link-orders", linkBody(created2.OrderID, 100000, 100), nil)
	require.Equal(t, http.StatusOK, recL2.Code, recL2.Body.String())
	var linkOut2 struct {
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(recL2.Body.Bytes(), &linkOut2))
	require.False(t, linkOut2.Created)

	// 已关联订单重复追加 → 幂等 created=false（先于守恒）
	recL3 := doJSON(t, h, "POST", "/api/v1/invoices/INV-n05/link-orders", linkBody(order1, 1, 1), nil)
	require.Equal(t, http.StatusOK, recL3.Code, recL3.Body.String())
	var linkOut3 struct {
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(recL3.Body.Bytes(), &linkOut3))
	require.False(t, linkOut3.Created)

	// 守恒：关联金额超出该订单未开票额度 → 409（订单 3 发货 10 件 ×1000 = 10000，关联 20000）
	rec3 := doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到",
		"idempotency_key":"order:chang-an:n05:o3",
		"items":[{"id":"item-n05-3","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":10,"unit_price_cents":1000}]}`, nil)
	require.Equal(t, http.StatusOK, rec3.Code, rec3.Body.String())
	var created3 struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &created3))
	store.SeedPrepared("chang-an", created3.OrderID, "item-n05-3", 10)
	store.SeedShipped("chang-an", created3.OrderID, "item-n05-3", 10)
	recL3b := doJSON(t, h, "POST", "/api/v1/invoices/INV-n05/link-orders", linkBody(created3.OrderID, 20000, 20), nil)
	require.Equal(t, http.StatusConflict, recL3b.Code, recL3b.Body.String())

	// 非法参数 → 400
	recL4 := doJSON(t, h, "POST", "/api/v1/invoices/INV-n05/link-orders",
		`{"order_id":"","amount_cents":0,"qty":0}`, nil)
	require.Equal(t, http.StatusBadRequest, recL4.Code, recL4.Body.String())

	// 未知发票 → 404
	recL5 := doJSON(t, h, "POST", "/api/v1/invoices/INV-nope/link-orders", linkBody(created2.OrderID, 1, 1), nil)
	require.Equal(t, http.StatusNotFound, recL5.Code, recL5.Body.String())

	// 三金额分展：订单 2 发货 100000 / 开票 100000 / 未开票 0
	recA := doJSON(t, h, "GET", "/api/v1/orders/"+created2.OrderID+"/invoice-summary", "", nil)
	require.Equal(t, http.StatusOK, recA.Code, recA.Body.String())
	var amt struct {
		ShippedCents    int64 `json:"shipped_cents"`
		InvoicedCents   int64 `json:"invoiced_cents"`
		UninvoicedCents int64 `json:"uninvoiced_cents"`
	}
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &amt))
	require.Equal(t, int64(100000), amt.ShippedCents)
	require.Equal(t, int64(100000), amt.InvoicedCents)
	require.Equal(t, int64(0), amt.UninvoicedCents)

	// 仓管三金额 → 隐藏（CQ-03）
	recW := doJSON(t, h, "GET", "/api/v1/orders/"+created2.OrderID+"/invoice-summary", "",
		map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusOK, recW.Code, recW.Body.String())
	var hidden struct {
		AmountsHidden bool `json:"amounts_hidden"`
	}
	require.NoError(t, json.Unmarshal(recW.Body.Bytes(), &hidden))
	require.True(t, hidden.AmountsHidden)

	// 未知订单 → 404
	recA2 := doJSON(t, h, "GET", "/api/v1/orders/ORD-nope/invoice-summary", "", nil)
	require.Equal(t, http.StatusNotFound, recA2.Code, recA2.Body.String())
}

// itoa64 int64 → string（测试载荷构造）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
