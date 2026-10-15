package workflows

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// S-06 · 影子模式（docs/v0.1/12 §5/§10）：
// Timer 全部不调度；shadow-inject 注入无回单例外分支可确定性重放；非影子模式行为不变。
//
// 判定口径：影子模式全链跑完 approval 通知数必须 = 6（六个闸门各一次创建通知）；
// 任一催办/回款 keep-alive Timer 被调度都会让计数 >6（escalationAction 也会发通知）。
// 信号刻意安排在 ApprovalRemindAfter 之后到达：若 Timer 未抑制，1h 处必然出现催办。

func TestS06ShadowModeTimersSuppressedFullChain(t *testing.T) {
	ord := &repo.Order{ID: "ORD-S1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	params := OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-S1",
		ApprovalRemindAfter:   time.Hour, // 信号在 2h 才到：Timer 若被调度，1h 必催办
		ApprovalEscalateAfter: 2 * time.Hour,
		ReceiptExceptionAfter: 3 * time.Hour,
		Shadow:                true,
	}
	le.at(2*time.Hour, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(3*time.Hour, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(4*time.Hour, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(5*time.Hour, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(6*time.Hour, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(7*time.Hour, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(8*time.Hour, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, params)
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Equal(t, 6, le.notif.count("approval:"), "shadow mode must not schedule timers")
}

// shadow-inject 注入无回单例外分支（真实 Timer 触发事件的等价信号）。
func TestS06ShadowInjectReceiptExceptionBranch(t *testing.T) {
	ord := &repo.Order{ID: "ORD-S2", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "月结"}
	le := newL2(t, ord)
	params := OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-S2",
		ApprovalRemindAfter:   time.Hour,
		ReceiptExceptionAfter: 100 * time.Hour, // 影子模式下绝不应由 Timer 触发
		Shadow:                true,
	}
	le.at(2*time.Hour, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(3*time.Hour, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(4*time.Hour, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(5*time.Hour, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(6*time.Hour, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	// 影子模式：无回单分支由 feed 投递 shadow-inject 等价信号（kind=receipt-exception）
	le.at(7*time.Hour, SignalShadowInject, ShadowInjectSignal{Kind: "unknown-kind"}) // 未知注入必须被忽略
	le.at(8*time.Hour, SignalShadowInject, ShadowInjectSignal{Kind: "receipt-exception"})
	le.at(9*time.Hour, SignalReceiptExReview, ReceiptExReviewSignal{Verdict: "APPROVED", Operator: "u-op"})
	le.at(10*time.Hour, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(11*time.Hour, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, params)
	require.Equal(t, domain.StateINVClosed, le.state())
	// 事件链含 ReceiptException：证明分支由 shadow-inject 注入（100h Timer 不会在 10h 前触发）
	require.True(t, le.store.hasEvent("ORD-S2", "ReceiptException"), "receipt exception event missing")
	// 月结路径闸门：PRICE + STOCK_REVIEW + RECEIPT_EX + INVOICE_FINANCE_ISSUE = 4（无到账确认/仓管任务卡）
	require.Equal(t, 4, le.notif.count("approval:"))
}

// 非影子模式行为不变：Shadow=false 且不传 Timer 时长 → 原测试基线全绿（回归哨兵）。
func TestS06NonShadowRegression(t *testing.T) {
	ord := &repo.Order{ID: "ORD-S3", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.complete(t)
	le.run(t, le.params("chang-an", "ORD-S3"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 六同原则 · 状态机等效对拍（D-011）：同一信号序列，Shadow=true 与 Shadow=false
// 产生完全相同的事件链与终态——证明影子不是另一套实现，只是 Timer/出站不同。
func TestShadowParityWithNonShadow(t *testing.T) {
	run := func(orderID string, shadow bool) *l2Env {
		ord := &repo.Order{ID: orderID, TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
		le := newL2(t, ord)
		le.complete(t)
		le.run(t, OrderWorkflowParams{
			TenantID: "chang-an", OrderID: orderID,
			ApprovalRemindAfter: 24 * time.Hour, ReceiptExceptionAfter: 120 * time.Hour,
			Shadow: shadow,
		})
		return le
	}
	a := run("ORD-P1", false)
	b := run("ORD-P2", true)
	require.Equal(t, a.state(), b.state())
	require.Equal(t, a.store.events, b.store.events)
}

// hasEvent 事件链断言辅助（events 形如 "ReceiptException:INV_READY_FOR_INVOICE->INV_RECEIPT_EXCEPTION_REVIEWING"）。
func (f *FakeStore) hasEvent(orderID, action string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := action + ":"
	for _, e := range f.events {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
