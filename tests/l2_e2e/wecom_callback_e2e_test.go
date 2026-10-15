//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/tests/testkit"
)

// TestE2EWeComCallbackAdapter W5 · 企微回调 → HTTP API 动作转接（07 §4.2）
//
// 证明「企微接入后」与「直接 HTTP」等价：回调 Handler（EventKey 协议）经真实 HTTP
// （HTTPActionClient → httptest 真实服务）驱动流程，与 tests/l2_e2e 直接信号同效。
// 验签本身由 W1 门禁覆盖（TestW1*），本用例从「验签后 payload」开始。
func TestE2EWeComCallbackAdapter(t *testing.T) {
	e := testkit.NewEnv(t)
	ctx := context.Background()

	// 真实 HTTP 服务（与企微转接层同一调用面）
	ts := httptest.NewServer(e.Server.Routes())
	defer ts.Close()

	handler := &wecom.CallbackHandler{
		Actions: &wecom.HTTPActionClient{BaseURL: ts.URL, HTTP: ts.Client()},
		Tasks:   e.Store, // repo.Repo 满足 wecom.TaskStore
		Logf:    t.Logf,
	}

	// ① 创建订单（直接 HTTP）
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-w5-cb-1")

	// ② 回调驱动审价闸门：approve:<taskID>（taskID 由库中 PENDING 任务定位）
	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	priceTask := pendingTaskID(t, e, orderID, "PRICE")
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:" + priceTask, FromUserID: "u-supervisor",
	}))
	// 0.1 商务备货闸门（选仓）：备货通过必须携带选仓，走 H5 表单信号
	// （stock-review；卡片 approve 不适用——见 wecom.stageSignal 注释）
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)

	// ③ 仓管闸门：仓管点击「通过」
	whTask := pendingTaskID(t, e, orderID, "STOCK_WAREHOUSE")
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:" + whTask, FromUserID: "u-wh-yangyuan",
	}))
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)

	// ④ 财务闸门
	payTask := pendingTaskID(t, e, orderID, "PAYMENT")
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:" + payTask, FromUserID: "u-accounting",
	}))
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)

	// ⑤ 发货（数量型操作，卡片不适用——07 §4.2 经 HTTP/S1 表单）→ 与回调驱动同效
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)
	e.WaitOrderState(orderID, domain.StateINVInvoicing)

	// ⑥ 开票闸门：approve 回调驱动
	invTask := pendingTaskID(t, e, orderID, "INVOICE_FINANCE_ISSUE")
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:" + invTask, FromUserID: "u-accounting",
	}))
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)

	// ⑦ 到账确认（信号名无审批任务；直发——与 E2E 全链一致）
	e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVClosed)

	// ⑧ 驳回路径：reject 回调 → 终态（同 Env 第二个订单需唯一 item ID）
	rec2 := e.DoJSON("POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:e2e-w5-reject-1",
		"items":[{"id":"item-2","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`,
		http.StatusOK, nil)
	var ord2 struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &ord2))
	orderID2 := ord2.OrderID
	e.WaitOrderState(orderID2, domain.StateORDPriceReviewing)
	task2 := pendingTaskID(t, e, orderID2, "PRICE")
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "reject:" + task2, FromUserID: "u-supervisor",
	}))
	e.WaitOrderState(orderID2, domain.StateORDRejected)

	// ⑨ 预订单确认回调：preorder:confirm:<id> → 派生正式订单
	rec := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-w5-msg-1","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`,
		http.StatusOK, nil)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:confirm:" + pre.PreOrderID, FromUserID: "dave-sales",
	}))
	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, pre.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreOrderRequested
	}, 10*time.Second, 50*time.Millisecond)

	// ⑩ 未知 EventKey → 拒绝（不落业务）
	err := handler.Handle(ctx, wecom.CallbackPayload{TenantID: "chang-an", EventKey: "bogus:xyz"})
	require.Error(t, err)

	// ⑩b 修改后确认回调：preorder:modify → confirmed=false → PRE_MODIFIED → 派生订单
	recM := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-w5-msg-2","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`,
		http.StatusOK, nil)
	var preM struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(recM.Body.Bytes(), &preM))
	require.NoError(t, handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:modify:" + preM.PreOrderID, FromUserID: "dave-sales",
	}))
	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, preM.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreOrderRequested
	}, 10*time.Second, 50*time.Millisecond)

	// ⑪ 幂等回执：已决任务再点 → 动作失败（409 透传为 error，企微侧可重试不重复推进）
	err = handler.Handle(ctx, wecom.CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:" + priceTask, FromUserID: "u-supervisor",
	})
	require.Error(t, err)
	// 状态未回退（仍在终态链上）
	require.Equal(t, domain.StateINVClosed, e.OrderState(orderID))
}

// pendingTaskID 查询订单当前 PENDING 审批任务 ID（回调定位用）。
func pendingTaskID(t *testing.T, e *testkit.Env, orderID, kind string) string {
	t.Helper()
	var id string
	require.Eventually(t, func() bool {
		err := e.Conn().QueryRow(t.Context(),
			`SELECT id FROM approval_tasks WHERE tenant_id='chang-an' AND order_id=$1 AND kind=$2 AND state='PENDING' LIMIT 1`,
			orderID, kind).Scan(&id)
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)
	return id
}
