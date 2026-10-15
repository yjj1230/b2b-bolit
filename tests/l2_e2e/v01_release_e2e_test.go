//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/tests/testkit"
)

// ── 0.1 发布版 E2E（docs/v0.1/09-testing N-02/N-05；真实装配 + 真 PG）──

// N-02 · 智能下单确认一次成单（XS3）：销售唯一确认角色（403）；确认派生订单只发生一次
// （重复确认不重复建单）。
func TestV01N02PreOrderConfirmOnce(t *testing.T) {
	e := testkit.NewEnv(t)
	rec := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"m-n02",
		"intent_version":"v1","sender_id":"u-sales",
		"candidates":[{"product_id":"prod-c68","quantity":50,"unit_price_cents":3200,"confidence":0.95,"customer_id":"cust-0001"}]}`,
		http.StatusOK, nil)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))

	// 非销售角色确认 → 403（N-02）
	_ = e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/preorder-confirm", pre.PreOrderID),
		`{"confirmed":true}`, http.StatusForbidden, map[string]string{"X-User-Id": "u-wh-yangyuan"})

	// 销售确认 → 派生正式订单
	e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/preorder-confirm", pre.PreOrderID),
		`{"confirmed":true,"operator":"dave-sales"}`, http.StatusOK, map[string]string{"X-User-Id": "dave-sales"})

	require.Eventually(t, func() bool {
		var n int
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key LIKE 'order:chang-an:pre:%'`).Scan(&n); err != nil {
			return false
		}
		return n == 1
	}, 10*time.Second, 50*time.Millisecond)

	// 重复确认 → 409 幂等回执（确认只发生一次）
	_ = e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/preorder-confirm", pre.PreOrderID),
		`{"confirmed":true,"operator":"dave-sales"}`, http.StatusConflict, map[string]string{"X-User-Id": "dave-sales"})
	time.Sleep(300 * time.Millisecond)
	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key LIKE 'order:chang-an:pre:%'`).Scan(&n))
	require.Equal(t, 1, n, "confirm must derive order exactly once")
}

// N-05 · 多单合并开票（A4）：两单走完发货 → 开票 → API 追加关联 → 三金额分展。
func TestV01N05MergeInvoiceE2E(t *testing.T) {
	e := testkit.NewEnv(t)
	mk := func(key, itemID string) string {
		orderID := e.CreateOrderWithItem("款到", key, itemID)
		e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
		e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
		e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
		e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
		e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
		e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
		e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
		e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)
		e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
		e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)
		return orderID
	}
	order1 := mk("order:chang-an:quick:e2e-v01n05-1", "item-n05-1")
	order2 := mk("order:chang-an:quick:e2e-v01n05-2", "item-n05-2")

	// 订单 1 开票（主关联）
	e.WaitOrderState(order1, domain.StateINVInvoicing)
	e.Signal(order1, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(order1, domain.StateINVPaymentTracking)

	var invoiceID string
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT id FROM invoices WHERE tenant_id='chang-an' AND order_id=$1 LIMIT 1`, order1).Scan(&invoiceID))

	// 追加关联订单 2（A4：漏了可补）
	linkBody := fmt.Sprintf(`{"order_id":%q,"amount_cents":100000,"qty":100}`, order2)
	rec := e.DoJSON("POST", fmt.Sprintf("/api/v1/invoices/%s/link-orders", invoiceID),
		linkBody, http.StatusOK, map[string]string{"X-User-Id": "u-op"})
	var linkOut struct {
		OK      bool `json:"ok"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &linkOut))
	require.True(t, linkOut.OK)
	require.True(t, linkOut.Created)

	// 三金额分展：订单 2 发货 100000 / 开票 100000 / 未开票 0
	recAmt := e.DoJSON("GET", "/api/v1/orders/"+order2+"/invoice-summary", "", http.StatusOK,
		map[string]string{"X-User-Id": "u-op"})
	var amt struct {
		ReceivedCents   int64 `json:"received_cents"`
		ShippedCents    int64 `json:"shipped_cents"`
		InvoicedCents   int64 `json:"invoiced_cents"`
		UninvoicedCents int64 `json:"uninvoiced_cents"`
	}
	require.NoError(t, json.Unmarshal(recAmt.Body.Bytes(), &amt))
	require.Equal(t, int64(100000), amt.ShippedCents)
	require.Equal(t, int64(100000), amt.InvoicedCents)
	require.Equal(t, int64(0), amt.UninvoicedCents)
}
