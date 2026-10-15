//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/tests/testkit"
)

// ── 0.1 发布版 E2E 补充（N-01 守门员 / N-06 凭证金额守恒；真 PG）──

// N-01 · 发群守门员（A3）：未确认不发送；确认后发送 + GatekeeperConfirmed 事件落库；
// 重复确认幂等（不重复发送/不重复记事件）；非销售 403。
func TestV01N01GatekeeperE2E(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-v01n01")

	path := fmt.Sprintf("/api/v1/orders/%s/send-to-group", orderID)
	headers := map[string]string{"X-User-Id": "dave-sales"}
	body := `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":false}`

	// 未确认 → 不发送、不记事件
	rec1 := e.DoJSON("POST", path, body, http.StatusOK, headers)
	var r1 struct {
		OK                   bool `json:"ok"`
		ConfirmationRequired bool `json:"confirmation_required"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &r1))
	require.False(t, r1.OK)
	require.True(t, r1.ConfirmationRequired)
	require.Empty(t, e.GroupGateway.SentGroupCards())

	// 确认 → 发送 + 事件
	rec2 := e.DoJSON("POST", path, `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":true}`, http.StatusOK, headers)
	var r2 struct {
		Sent bool `json:"sent"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &r2))
	require.True(t, r2.Sent)
	require.Len(t, e.GroupGateway.SentGroupCards(), 1)

	// 重复确认 → 幂等（不重复发送/事件只一条）
	rec3 := e.DoJSON("POST", path, `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":true}`, http.StatusOK, headers)
	var r3 struct {
		AlreadyConfirmed bool `json:"already_confirmed"`
		Sent             bool `json:"sent"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &r3))
	require.True(t, r3.AlreadyConfirmed)
	require.False(t, r3.Sent)
	require.Len(t, e.GroupGateway.SentGroupCards(), 1)
	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM order_events WHERE tenant_id='chang-an' AND order_id=$1 AND action='GatekeeperConfirmed'`, orderID).Scan(&n))
	require.Equal(t, 1, n, "GatekeeperConfirmed 事件只记一次（append-only 审计）")

	// 非销售 → 403
	_ = e.DoJSON("POST", path, `{"group_id":"g","content":"x","confirm":true}`,
		http.StatusForbidden, map[string]string{"X-User-Id": "u-wh-yangyuan"})
}

// N-06 · 凭证上传主路径 + 金额守恒（A2）：金额 = 已发货金额通过；超限 409 拒绝；
// 识别登记字段（kind/source/invoice_no）落库；追加 v2 不覆盖 v1。
func TestV01N06EvidenceAmountE2E(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-v01n06")

	e.WaitOrderState(orderID, "ORD_PRICE_REVIEWING")
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(orderID, "STK_STOCK_REVIEWING")
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, "STK_WAREHOUSE_CONFIRMING")
	e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, "STK_PAYMENT_CONFIRMING")
	e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, "SHP_SHIP_ASSIGNING")
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)

	// 发货单落库后取证（无需等后续段推进）
	var shipmentID string
	require.Eventually(t, func() bool {
		return e.Conn().QueryRow(t.Context(),
			`SELECT id FROM shipments WHERE tenant_id='chang-an' AND order_id=$1 LIMIT 1`, orderID).Scan(&shipmentID) == nil
	}, 10*time.Second, 50*time.Millisecond)
	path := fmt.Sprintf("/api/v1/orders/%s/shipments/%s/evidence", orderID, shipmentID)

	// 金额 = 已发货金额（100 × 1000 = 100000）→ 201 v1
	rec1 := e.DoJSON("POST", path, `{"file_url":"file://inv1.pdf","file_hash":"h1",
		"kind":"INVOICE","source":"manual","invoice_no":"NO 04511230","amount_cents":100000,"tax_cents":13000}`,
		http.StatusCreated, map[string]string{"X-User-Id": "u-accounting"})
	var v1 struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &v1))
	require.Equal(t, 1, v1.Version)

	// 超限 100001 → 409（N-06 守恒）
	_ = e.DoJSON("POST", path, `{"file_url":"file://inv2.pdf","file_hash":"h2","kind":"INVOICE","invoice_no":"NO 04511230-2","amount_cents":100001}`,
		http.StatusConflict, map[string]string{"X-User-Id": "u-accounting"})

	// 追加 v2（robot 兜底路径，不同发票号）→ 只追加不覆盖
	rec3 := e.DoJSON("POST", path, `{"file_url":"file://inv2.pdf","file_hash":"h2",
		"kind":"INVOICE","source":"robot","invoice_no":"NO 04511230-B","amount_cents":100000}`,
		http.StatusCreated, map[string]string{"X-User-Id": "u-accounting"})
	var v2 struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &v2))
	require.Equal(t, 2, v2.Version)

	// DB 断言：v1/v2 保留 + 识别登记字段落库
	var cnt int
	var kind, source, invoiceNo string
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM evidence_versions WHERE tenant_id='chang-an' AND shipment_id=$1`, shipmentID).Scan(&cnt))
	require.Equal(t, 2, cnt)
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT kind, source, invoice_no FROM evidence_versions WHERE tenant_id='chang-an' AND shipment_id=$1 AND version=2`, shipmentID).
		Scan(&kind, &source, &invoiceNo))
	require.Equal(t, "INVOICE", kind)
	require.Equal(t, "robot", source)
	require.Equal(t, "NO 04511230-B", invoiceNo)
}
