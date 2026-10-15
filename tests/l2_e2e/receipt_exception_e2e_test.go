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

// TestE2EPreOrderEscalateAndConfirm 预订单升级 → 降级 → 确认（CQ-11；升级后仍可人工确认）。
//
// TestSuite 虚拟时钟自动触发超期 Timer → PRE_ESCALATED；随后销售确认 →
// 归一化 PENDING_CONFIRM → 派生订单。重复升级（Timer 再次触发）不产生非法自迁移。
func TestE2EPreOrderEscalateAndConfirm(t *testing.T) {
	e := testkit.NewEnv(t)

	rec := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-msg-esc","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`,
		http.StatusOK, nil)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))

	// 不确认 → 超期 Timer 自动触发 → 升级（分流：超期未处理）
	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, pre.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreEscalated
	}, 10*time.Second, 50*time.Millisecond)

	// 升级后确认 → 归一化降级 → 派生订单（升级不阻塞人工决策）
	e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/%s", pre.PreOrderID, "preorder-confirm"),
		`{"confirmed":true,"operator":"u-sales"}`, http.StatusOK, map[string]string{"X-User-Id": "dave-sales"})

	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, pre.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreOrderRequested
	}, 10*time.Second, 50*time.Millisecond)

	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key LIKE 'order:chang-an:pre:%'`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestE2EIdempotentReplayThroughHTTP L2-01 经 HTTP · 幂等重放：同键下单返回既有订单，流程不重复推进。
func TestE2EIdempotentReplayThroughHTTP(t *testing.T) {
	e := testkit.NewEnv(t)
	body := `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:e2e-replay-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`
	rec := e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o1 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &o1))
	require.True(t, o1.Created)

	// 流程推进后重放同键 → 同一订单、不重复启动
	e.WaitOrderState(o1.OrderID, domain.StateORDPriceReviewing)
	rec = e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o2 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &o2))
	require.False(t, o2.Created)
	require.Equal(t, o1.OrderID, o2.OrderID)

	// 重放后流程仍可推进至终态（0.1：备货选仓 → 仓管确认）
	e.Signal(o1.OrderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(o1.OrderID, domain.StateSTKStockReviewing)
	e.Signal(o1.OrderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(o1.OrderID, domain.StateSTKWarehouseConfirm)
}
