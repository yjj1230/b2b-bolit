//go:build integration

// Package e2e —— L2 · HTTP API 端到端（docs/03-testing.html L2）
//
// 真实装配：api.NewServer + api.LocalRunner（进程内真实 OrderWorkflow）+ 真实
// PostgreSQL + StaticChecker + wecom mock 出站。无 Temporal/SpiceDB 服务依赖。
// 每个用例：schema 重置 → 迁移 → 种子世界 → HTTP 驱动全业务流。
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

// ── 常用信号载荷（与 internal/workflows 信号结构一致）──

const (
	sigPriceReview      = "price-review"
	sigChairmanReview   = "chairman-review"
	sigStockReview      = "stock-review"
	sigWarehouseConfirm = "warehouse-confirm"
	sigPaymentConfirm   = "payment-confirm"
	sigShipAction       = "ship-action"
	sigReceiptProvided  = "receipt-provided"
	sigInvoiceIssued    = "invoice-issued"
	sigPaymentTracked   = "payment-tracked"
	sigModifyOrder      = "modify-order"
)

// TestE2EFullChainPaid L2-01 · 快速下单 happy path：ORD→STK→SHP→INV→CLOSED 全链（CQ-04）。
func TestE2EFullChainPaid(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-paid-1")

	// ORD：审价
	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)

	// STK：商务备货（选仓）→ 仓管确认 → 财务到账（款到分流）
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
	e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
	e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)

	// SHP：实发 100（库存联动）
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)

	// INV：款到直接开票（分流①）→ 开票完成 → 回款 → 关闭
	e.WaitOrderState(orderID, domain.StateINVInvoicing)
	e.Signal(orderID, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
	e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)

	e.WaitOrderState(orderID, domain.StateINVClosed)

	// CQ-12 事件链：终态订单事件可回放且非空
	require.Greater(t, e.EventCount(orderID), 10)

	// CQ-07/08 守恒终态：0.1 M1 锁库存暂缓 → 库存不变 1000；发货 100；开票 100
	var qty int64
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT qty FROM inventory_items WHERE tenant_id='chang-an' AND warehouse_id='wh-yangyuan' AND product_id='prod-c68'`).Scan(&qty))
	require.Equal(t, int64(1000), qty)
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COALESCE(SUM(shipped_qty),0) FROM shipments WHERE tenant_id='chang-an' AND order_id=$1`, orderID).Scan(&qty))
	require.Equal(t, int64(100), qty)
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COALESCE(SUM(ii.qty),0) FROM invoice_items ii
		 JOIN order_items oi ON oi.tenant_id=ii.tenant_id AND oi.id=ii.order_item_id
		 WHERE ii.tenant_id='chang-an' AND oi.order_id=$1`, orderID).Scan(&qty))
	require.Equal(t, int64(100), qty)

	// CQ-05 待办：全部裁决 → 无 PENDING
	rec := e.DoJSON("GET", "/api/v1/todos?state=PENDING", "", http.StatusOK, nil)
	var todos struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &todos))
	require.Empty(t, todos.Tasks)
}

// TestE2EFullChainNonPaid L2-07 · 非款到发货：跳过财务确认；无回单超期 → 例外审核 → 开票（CQ-05/10）。
//
// 说明：LocalRunner 的 TestSuite 虚拟时钟在工作流阻塞时自动触发 Timer（无信号竞争时），
// 故「无回单超期」在 E2E 中确定性地落入例外审核（分流③）；「补回单立即开票」
// （分流②）依赖精确时间点，由 internal/workflows TestSuite（L2-07 虚拟时间）覆盖。
// 时长语义为虚拟 10s（integration.NewDemoPolicy），业务路径与生产 5 天一致。
func TestE2EFullChainNonPaid(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("非款到", "order:chang-an:quick:e2e-nonpaid-1")

	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)

	// 非款到：备货审批跳过 → 直达发货分配
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)

	// 无回单 + 超期 Timer 自动触发 → 例外审核（分流③）→ 通过 → 开票
	e.WaitOrderState(orderID, domain.StateINVReceiptExReviewing)
	e.Signal(orderID, "receipt-ex-review", `{"verdict":"APPROVED","operator":"u-accounting"}`)

	e.WaitOrderState(orderID, domain.StateINVInvoicing)
	e.Signal(orderID, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
	e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)

	e.WaitOrderState(orderID, domain.StateINVClosed)

	// 无财务确认任务（跳过 PAYMENT 闸门）
	rec := e.DoJSON("GET", "/api/v1/todos?kind=PAYMENT", "", http.StatusOK, nil)
	var todos struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &todos))
	require.Empty(t, todos.Tasks)

	// 例外审核事件入链（CQ-12 审计留痕）
	rec = e.DoJSON("GET", "/api/v1/orders/"+orderID+"/events", "", http.StatusOK, nil)
	var evs struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &evs))
	found := false
	for _, ev := range evs.Events {
		if ev.Action == "ReceiptException" || ev.Action == "ReceiptExceptionApproved" {
			found = true
		}
	}
	require.True(t, found, "exception review must be recorded in event chain")
}

// TestE2EChairmanCD L2-04 · C/D 级客户董事长审批链（通过；CQ-05）。
func TestE2EChairmanCD(t *testing.T) {
	e := testkit.NewEnv(t)
	rec := e.DoJSON("POST", "/api/v1/orders", `{
		"customer_id":"cust-0002","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:e2e-cd-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`,
		http.StatusOK, nil)
	var out struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	orderID := out.OrderID

	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)

	// C 级 → 董事长闸门
	e.WaitOrderState(orderID, domain.StateORDChairmanReview)
	e.Signal(orderID, sigChairmanReview, `{"verdict":"APPROVED","operator":"u-chairman"}`)

	// 商务备货（选仓）→ 仓管确认
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
	e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
	e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)
	e.WaitOrderState(orderID, domain.StateINVInvoicing)
	e.Signal(orderID, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
	e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)

	e.WaitOrderState(orderID, domain.StateINVClosed)
}

// TestE2EPartialShipment L2-08 · 部分发货→按计划继续→累计满额→全部发货（CQ-07）。
func TestE2EPartialShipment(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-partial-1")

	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
	e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
	e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)

	// 第一批发 40
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":40}`)
	e.WaitOrderState(orderID, domain.StateSHPPartialShipped)

	// 第二批补足 60：部分发货后按原分批计划继续（闸门在 PARTIAL_SHIPPED 状态；
	// 间隔 300ms 投递，避免与段重启处理重叠的信号递送竞态）
	time.Sleep(300 * time.Millisecond)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":60}`)
	e.WaitOrderState(orderID, domain.StateINVInvoicing)

	e.Signal(orderID, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
	e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVClosed)

	// 两批发货单、累计 100
	var n, sum int64
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*), COALESCE(SUM(shipped_qty),0) FROM shipments WHERE tenant_id='chang-an' AND order_id=$1`, orderID).Scan(&n, &sum))
	require.Equal(t, int64(2), n)
	require.Equal(t, int64(100), sum)
}

// TestE2EModificationClasses L2-02 · A/B/C/D/E 修改分类：均生成 revision 与 resubmit 链（CQ-04/05/12）。
func TestE2EModificationClasses(t *testing.T) {
	cases := []struct {
		class string
		extra func(e *testkit.Env, orderID string) // 分类特有信号（A/C 重走审价/合同）
	}{
		{"A", func(e *testkit.Env, orderID string) {
			e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
			e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
		}},
		{"B", nil},
		{"C", nil},
		{"D", nil},
		{"E", nil},
	}
	for _, c := range cases {
		t.Run("class-"+c.class, func(t *testing.T) {
			e := testkit.NewEnv(t)
			orderID := e.CreateOrder("款到", fmt.Sprintf("order:chang-an:quick:e2e-mod-%s", c.class))

			// 停在 STK 备货闸门（STOCK_REVIEWING）→ 修改请求
			e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
			e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
			e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
			e.Signal(orderID, sigModifyOrder, fmt.Sprintf(`{"class":%q,"operator":"u-sales","changes":["x"]}`, c.class))

			// 分类特有路径
			if c.extra != nil {
				c.extra(e, orderID)
			}

			switch c.class {
			case "B", "D":
				// 直达发货：修改重启落定在 SHP 闸门；间隔 300ms 投递避免重启竞态
				e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
				time.Sleep(300 * time.Millisecond)
			default: // A/C/E
				// 修改重启落定到备货闸门（STOCK_REVIEWING）后再投递信号。
				// TestSuite 信号递送与工作流重启处理存在竞态，间隔 300ms 投递
				e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
				time.Sleep(300 * time.Millisecond)
				e.SignalRetry(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
				e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
				time.Sleep(300 * time.Millisecond)
				e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
				time.Sleep(300 * time.Millisecond)
				e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)
				e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
			}
			e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)

			e.WaitOrderState(orderID, domain.StateINVInvoicing)
			e.Signal(orderID, sigInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`)
			e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
			e.Signal(orderID, sigPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`)
			e.WaitOrderState(orderID, domain.StateINVClosed)

			// resubmit 链（CQ-12）：revision=1、mode=resubmit、from/root 非空
			var revision int
			var mode, fromID string
			require.NoError(t, e.Conn().QueryRow(t.Context(),
				`SELECT resubmit_revision, submission_mode, resubmit_from_order_id FROM orders WHERE id=$1`, orderID).
				Scan(&revision, &mode, &fromID))
			require.Equal(t, 1, revision)
			require.Equal(t, "resubmit", mode)
			require.NotEmpty(t, fromID)
		})
	}
}

// TestE2ERejectPath L2-03 · 主管拒绝 → 订单 REJECTED 终态（资金无副作用）。
func TestE2ERejectPath(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-reject-1")

	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"REJECTED","operator":"u-supervisor","reason":"价格过高"}`)

	e.WaitOrderState(orderID, domain.StateORDRejected)
	// 终态稳定：停留后不再推进
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, domain.StateORDRejected, e.OrderState(orderID))
	// 无发货/开票副作用
	var n int64
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM shipments WHERE tenant_id='chang-an' AND order_id=$1`, orderID).Scan(&n))
	require.Zero(t, n)
}

// TestE2ESignalErrors 信号错误路径：未知信号 400；不存在的订单 404。
func TestE2ESignalErrors(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-sigerr-1")

	rec := e.Do("POST", fmt.Sprintf("/api/v1/orders/%s/signal/bogus", orderID), `{}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = e.Do("POST", "/api/v1/orders/ORD-nope/signal/price-review", `{"verdict":"APPROVED","operator":"u-supervisor"}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 空载荷信号（如 receipt-provided）→ 400（非法 JSON）
	rec = e.Do("POST", fmt.Sprintf("/api/v1/orders/%s/signal/receipt-provided", orderID), ``, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestE2ETodosDuringFlow CQ-05 · 流程中待办按闸门出现并可检索。
func TestE2ETodosDuringFlow(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-todos-1")

	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	// 状态迁移与审批任务创建分属两个事务：轮询直至任务可见
	var todos struct {
		Tasks []struct {
			Kind      string `json:"kind"`
			StageCode string `json:"stage_code"`
		} `json:"tasks"`
	}
	require.Eventually(t, func() bool {
		rec := e.DoJSON("GET", "/api/v1/todos?state=PENDING", "", http.StatusOK, nil)
		if err := json.Unmarshal(rec.Body.Bytes(), &todos); err != nil {
			return false
		}
		return len(todos.Tasks) > 0
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, "PRICE", todos.Tasks[0].Kind)

	// 按 assignee 检索（审批链节点）
	rec := e.DoJSON("GET", "/api/v1/todos?assignee=u-supervisor&state=PENDING", "", http.StatusOK, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &todos))
	require.NotEmpty(t, todos.Tasks)
}
