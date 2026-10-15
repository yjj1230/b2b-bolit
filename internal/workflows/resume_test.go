package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── 段恢复分支覆盖（workflow 从中间状态启动 = 修改/重提/异常后的重入路径）──

// 从 STK 中间状态恢复：仓管退回后重提（stkResume RETURNED → 备货）
func TestResumeStkReturned(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateSTKReturned
	h := time.Hour
	le.at(h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(2*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(3*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(4*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(5*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 STK 财务确认后恢复（stkResume PAYMENT_CONFIRMED 场景）
func TestResumeStkPaymentConfirmed(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateSTKPaymentConfirmed
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 SHP 部分发货后恢复：继续发货到满额（shpContinueStep SHIP）
func TestResumeShpPartial(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateSHPPartialShipped
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 INV 回单等待恢复：补回单后进入开票（WAITING_RECEIPT → INVOICE_APPLYING）
func TestResumeInvWaitingReceipt(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateINVWaitingReceipt
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalReceiptProvided, nil)
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 INV 开票失败恢复：重试成功（INVOICE_FAILED → INVOICING）
func TestResumeInvInvoiceFailed(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateINVInvoiceFailed
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(2*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 回单例外拒绝 → 待补回单 → 补回单 → 开票（invExceptionStep REJECTED 分支）
func TestReceiptExceptionRejectedThenProvided(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	// 5 天 Timer → 例外 → 拒绝 → 待补回单
	le.at(122*h, SignalReceiptExReview, ReceiptExReviewSignal{Verdict: "REJECTED", Operator: "u-op"})
	le.at(123*h, SignalReceiptProvided, nil)
	le.at(124*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(125*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-1",
		ApprovalRemindAfter:   24 * time.Hour,
		ReceiptExceptionAfter: 120 * time.Hour,
	})
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Contains(t, le.store.events, "ReceiptExceptionRejected:INV_RECEIPT_EXCEPTION_REVIEWING->"+domain.StateINVWaitingReceipt)
}

// 从 STK_SHIP_ORDER_ISSUED 恢复（stkSegment → SHP）
func TestResumeStkShipOrderIssued(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateSTKShipOrderIssued
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 SHP_WAREHOUSE_SHIPPING 恢复（→ 发货完成判定）
func TestResumeShpWarehouseShipping(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateSHPWarehouseShip
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 INV_RECEIPT_EX_REVIEWING 恢复（例外审核中）
func TestResumeInvReceiptExReviewing(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateINVReceiptExReviewing
	le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
	le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
	h := time.Hour
	le.at(h, SignalReceiptExReview, ReceiptExReviewSignal{Verdict: "APPROVED", Operator: "u-op"})
	le.at(2*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(3*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
}

// 从 INV_CONTRACT_ARCHIVED 恢复（→ CLOSED）与 INV_CLOSED 终态幂等
func TestResumeInvArchivedAndClosed(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.store.order("chang-an", "ORD-1").State = domain.StateINVContractArchived
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())

	ord2 := &repo.Order{ID: "ORD-2", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le2 := newL2(t, ord2)
	le2.store.order("chang-an", "ORD-2").State = domain.StateINVClosed
	le2.run(t, le2.params("chang-an", "ORD-2"))
	require.Equal(t, domain.StateINVClosed, le2.state())

	// 拒绝终态幂等
	ord3 := &repo.Order{ID: "ORD-3", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le3 := newL2(t, ord3)
	le3.store.order("chang-an", "ORD-3").State = domain.StateORDRejected
	le3.run(t, le3.params("chang-an", "ORD-3"))
	require.Equal(t, domain.StateORDRejected, le3.state())
}

// fake 存储补充：ListPreOrderEvents/Orders/SeedTenant 直接覆盖
func TestFakeStoreExtras(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	store.SeedTenant("t")
	store.SeedCustomer("t", "c", "A")
	store.SeedInventory("t", "wh", "p", 10)
	po, _, err := store.CreatePreOrder(ctx, NewPreOrderForTest("t", "g", "m"))
	require.NoError(t, err)
	evs, err := store.ListPreOrderEvents(ctx, "t", po.ID)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	o, _, err := store.CreateOrder(ctx, NewOrderForTest("t", "c", "款到", "k-1"))
	require.NoError(t, err)
	require.Len(t, store.Orders(), 1)
	require.Equal(t, "ORD-"+shortFakeID("t:k-1"), o.ID)
}

// STK 各中间状态恢复（stkResume 全部分支）
func TestResumeStkMidStates(t *testing.T) {
	cases := []struct {
		name  string
		state string
		drive func(le *l2Env, h time.Duration)
	}{
		{"STOCK_REVIEWING", domain.StateSTKStockReviewing, func(le *l2Env, h time.Duration) {
			le.at(1*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
			le.at(1*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
			le.at(2*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
		}},
		{"WAREHOUSE_RETURNED", domain.StateSTKWarehouseReturned, func(le *l2Env, h time.Duration) {
			le.at(1*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
			le.at(1*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
			le.at(2*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
		}},
		{"TRANSFER_REQUESTED", domain.StateSTKTransferRequested, func(le *l2Env, h time.Duration) {
			le.at(1*h, SignalTransferDecision, TransferDecisionSignal{Verdict: "APPROVE_ORIGINAL", Operator: "u-op"})
			le.at(2*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
		}},
		{"WAREHOUSE_CONFIRMED", domain.StateSTKWarehouseConfirmed, func(le *l2Env, h time.Duration) {
			le.at(1*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
			le := newL2(t, ord)
			le.store.order("chang-an", "ORD-1").State = c.state
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			h := time.Hour
			c.drive(le, h)
			le.at(3*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
			le.at(4*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
			le.at(5*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
			le.run(t, le.params("chang-an", "ORD-1"))
			require.Equal(t, domain.StateINVClosed, le.state())
		})
	}
}

// 全状态启动：每个合法订单状态启动工作流并驱动到完成（覆盖段 dispatch 全部 case）
func TestResumeAllStates(t *testing.T) {
	drive := func(le *l2Env, seed func(le *l2Env), h time.Duration) {
		seed(le)
		le.at(1*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
		le.at(1*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
		le.at(2*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
		le.at(3*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
		le.at(4*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
		le.at(5*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
		le.run(t, le.params("chang-an", "ORD-1"))
		require.Equal(t, domain.StateINVClosed, le.state())
	}
	base := func(le *l2Env) {
		le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
	}
	_ = drive
	_ = base

	cases := []struct {
		name  string
		state string
		seed  func(le *l2Env)
	}{
		{"ORD_PRICE_APPROVED", domain.StateORDPriceApproved, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"ORD_CHAIRMAN_REVIEW", domain.StateORDChairmanReview, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.at(0, SignalChairmanReview, ChairmanReviewSignal{Verdict: "APPROVED", Operator: "u-chairman"})
		}},
		{"ORD_CONTRACT_GENERATING", domain.StateORDContractGenerate, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"STK_READY_FOR_STOCK", domain.StateSTKReadyForStock, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"STK_STOCK_REVIEWING", domain.StateSTKStockReviewing, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"STK_WAREHOUSE_CONFIRM", domain.StateSTKWarehouseConfirm, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"STK_PAYMENT_CONFIRMING", domain.StateSTKPaymentConfirming, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"SHP_FULLY_SHIPPED", domain.StateSHPFullyShipped, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
		{"SHP_PARTIAL_SHIPPED", domain.StateSHPPartialShipped, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"INV_READY_FOR_INVOICE", domain.StateINVReadyForInvoice, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
		{"INV_INVOICE_APPLYING", domain.StateINVInvoiceApplying, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
		{"INV_INVOICING", domain.StateINVInvoicing, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
		{"INV_PAYMENT_TRACKING", domain.StateINVPaymentTracking, func(le *l2Env) {
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			le.store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
			le := newL2(t, ord)
			le.store.order("chang-an", "ORD-1").State = c.state
			if c.seed != nil {
				c.seed(le)
			}
			drive(le, c.seed, time.Hour)
		})
	}
}
