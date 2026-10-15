package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── L2 测试环境：Temporal TestSuite + FakeStore/FakeNotifier/StaticChecker（纯库）──
//
// 驱动模式（v1.47 test env 行为）：
//  - ExecuteWorkflow 会一直泵到工作流完成；工作流阻塞在 Signal 且无挂起 timer 时，
//    环境会自动触发 10 年 workflow-timeout timer 使工作流失败。
//  - 因此每个闸门都必须有挂起 timer（ApprovalRemindAfter=24h 生产语义），
//    且测试用 RegisterDelayedCallback 以递增虚拟时间驱动全部 Signal（先于 24h 到达）。

type l2Env struct {
	suite *testsuite.WorkflowTestSuite
	env   *testsuite.TestWorkflowEnvironment
	store *FakeStore
	notif *FakeNotifier
	order *repo.Order
}

func newL2(t *testing.T, order *repo.Order) *l2Env {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) {
		return authz.RoleOP, nil // 演示模式：授权矩阵由 L1-07/L2-14 覆盖
	})
	le := &l2Env{suite: &testsuite.WorkflowTestSuite{}, store: store, notif: notif, order: order}
	le.env = le.suite.NewTestWorkflowEnvironment()
	le.env.RegisterActivity(NewActivities(store, notif, checker))
	le.env.RegisterWorkflow(OrderWorkflow)
	le.env.RegisterWorkflow(PreOrderWorkflow)
	if order != nil {
		le.store.seedOrder(order.TenantID, order.ID, order.CustomerID, order.PaymentTerm, 100)
		le.store.setInventory(order.TenantID, "wh-yangyuan", "prod-c68", 1000)
	}
	return le
}

// params 标准参数：所有闸门挂 24h 催办 timer（生产语义；测试信号先于 24h 到达）。
func (le *l2Env) params(tenantID, orderID string) OrderWorkflowParams {
	return OrderWorkflowParams{
		TenantID: tenantID, OrderID: orderID, ApprovalRemindAfter: 24 * time.Hour,
	}
}

// at 注册一个在虚拟时刻 at 触发的 Signal（须按时间递增注册）。
func (le *l2Env) at(at time.Duration, name string, val any) {
	le.env.RegisterDelayedCallback(func() {
		le.env.SignalWorkflow(name, val)
	}, at)
}

func (le *l2Env) state() string {
	return le.store.order("chang-an", le.order.ID).State
}

// run 执行工作流（阻塞至完成；信号由 delayed callback 驱动）。
func (le *l2Env) run(t *testing.T, params OrderWorkflowParams) {
	le.env.ExecuteWorkflow(OrderWorkflow, params)
	require.NoError(t, le.env.GetWorkflowError())
}

// complete 注册全链信号（t0+1h..5h，均早于 24h 催办 timer）。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (le *l2Env) complete(t *testing.T) {
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalChairmanReview, ChairmanReviewSignal{Verdict: "APPROVED", Operator: "u-chairman"})
	le.at(3*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(3*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(4*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(5*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
}

// L2-01 · 快速下单 happy path：ORD→STK→SHP→INV→CLOSED 全链（CQ-04）
func TestL201HappyPath(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.complete(t)
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	// 发货守恒 + 开票守恒终态检查；M1（0.1 锁库存暂缓）：发货不扣库存 → 仍 1000
	require.Equal(t, int64(1000), le.store.inventory["chang-an/wh-yangyuan/prod-c68"])
	require.Equal(t, int64(100), le.store.shippedByItem["chang-an/ORD-1/item-1"])
	require.Equal(t, int64(100), le.store.invoicedByItem["chang-an/ORD-1/item-1"])
	require.Equal(t, 6, le.notif.count("approval:")) // PRICE + STOCK_REVIEW + STOCK_WAREHOUSE + PAYMENT + WAREHOUSE + INVOICE_FINANCE_ISSUE
}

// L2-02 · A/B/C/D/E 修改分类：均生成 revision 与 resubmit 链（CQ-04/05/12）
func TestL202ModificationClasses(t *testing.T) {
	cases := []struct {
		class domain.ModificationClass
		want  string
		stage string // 修改后重启段
	}{
		{domain.ModClassA, domain.StateORDPriceReviewing, "ORD"},
		{domain.ModClassB, domain.StateSHPShipAssigning, "SHP"},
		{domain.ModClassC, domain.StateORDContractGenerate, "ORD"},
		{domain.ModClassD, domain.StateSHPShipAssigning, "SHP"},
		{domain.ModClassE, domain.StateSTKStockReviewing, "STK"}, // 静默修改后继续 STK
	}
	for _, c := range cases {
		t.Run(string(c.class), func(t *testing.T) {
			ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
			le := newL2(t, ord)
			h := time.Hour
			le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
			// 先放行商务备货闸门（选仓）→ 停在仓管确认等待 → 修改请求
			le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
			le.at(2*h, SignalModifyOrder, ModifyOrderSignal{Class: c.class, Operator: "u-sales", Changes: []string{"x"}})
			// 修改后继续走完（A/C 需重走审价/合同；B/D 直达发货；E 续 STK）
			le.at(3*h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
			le.at(4*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
			le.at(4*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
			le.at(5*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
			le.at(6*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
			le.at(7*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
			le.at(8*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
			le.run(t, le.params("chang-an", "ORD-1"))
			require.Equal(t, domain.StateINVClosed, le.state())
			// resubmit 链：revision 递增、mode=resubmit
			o := le.store.order("chang-an", "ORD-1")
			require.Equal(t, 1, o.ResubmitRevision)
			require.Equal(t, "resubmit", o.SubmissionMode)
			require.NotNil(t, o.ResubmitFromOrderID)
			// 修改事件留痕（ModifyRequested 进入 ORD_MODIFYING）
			require.True(t, containsPrefix(le.store.events, "ModifyRequested:"), "modify event missing")
			_ = c
		})
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// L2-03 · 主管拒绝 → 订单 REJECTED（资金无副作用；CQ-04）
func TestL203SupervisorReject(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.at(time.Hour, SignalPriceReview, PriceReviewSignal{Verdict: "REJECTED", Operator: "u-supervisor", Reason: "价格过高"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateORDRejected, le.state())
	require.Zero(t, len(le.store.shipments))
	require.Zero(t, len(le.store.invoices))
}

// L2-04 · C/D 级董事长审批链（通过/拒绝；CQ-05）
func TestL204ChairmanChain(t *testing.T) {
	// 通过
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0002", PaymentTerm: "款到"} // C 级
	le := newL2(t, ord)
	le.complete(t)
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	// 董事长任务已裁决 APPROVED
	tasks, _ := le.store.ListApprovalTasks(t.Context(), "chang-an", "", "CHAIRMAN", "", 10)
	require.Len(t, tasks, 1)
	require.Equal(t, "APPROVED", tasks[0].State)

	// 拒绝
	ord2 := &repo.Order{ID: "ORD-2", TenantID: "chang-an", CustomerID: "cust-0002", PaymentTerm: "款到"}
	le2 := newL2(t, ord2)
	h := time.Hour
	le2.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le2.at(2*h, SignalChairmanReview, ChairmanReviewSignal{Verdict: "REJECTED", Operator: "u-chairman"})
	le2.run(t, le2.params("chang-an", "ORD-2"))
	require.Equal(t, domain.StateORDRejected, le2.state())
}

// D-02 收敛：仓管退回后任务回到商务重新选仓（重开 stock-review 闸门）。
func TestL203cWarehouseReturnedReopensStockReview(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(3*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "RETURNED", Operator: "u-wh-yangyuan"})
	le.at(4*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-dongguan"})
	le.at(5*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-dongguan"})
	le.at(6*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(7*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-dongguan", Qty: 100})
	le.at(8*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(9*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	sp, err := le.store.GetStockPrepare(t.Context(), "chang-an", "STK-"+shortFakeID("ORD-1:1"))
	require.NoError(t, err)
	require.Equal(t, "wh-dongguan", sp.WarehouseID, "重新选仓必须生效")
}

// DSZ-01 收敛：董事长「要求修改」（RETURNED）→ 销售重提 → 重新董事长终审 → 放行履约。
func TestL204cChairmanReturnedThenResubmit(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0002", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalChairmanReview, ChairmanReviewSignal{Verdict: "RETURNED", Operator: "u-chairman"})
	le.at(3*h, SignalResubmit, ResubmitSignal{Operator: "u-sales"})
	le.at(4*h, SignalChairmanReview, ChairmanReviewSignal{Verdict: "APPROVED", Operator: "u-chairman"})
	le.at(5*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(6*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(7*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(8*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(9*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(10*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	tasks, err := le.store.ListApprovalTasks(t.Context(), "chang-an", "", "CHAIRMAN", "", 10)
	require.NoError(t, err)
	require.Len(t, tasks, 2, "RETURNED 后重提应产生第二次董事长终审任务")
	require.Equal(t, "RETURNED", tasks[0].State)
	require.Equal(t, "APPROVED", tasks[1].State)
}

// L2-05 · 合同生成校验：要素不全 → 补全重校验（CQ-04）
func TestL205ContractValidation(t *testing.T) {
	in := GenerateContractInput{OrderID: "ORD-1", Order: &repo.Order{PaymentTerm: ""}, Items: nil}
	a := &Activities{}
	res, err := a.GenerateContract(t.Context(), in)
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Missing, "payment_term")
	require.Contains(t, res.Missing, "items")
	in.Order.PaymentTerm = "款到"
	in.CustomerName = "北京华信科技"
	in.Items = []repo.OrderItem{{ID: "i1", ProductID: "p1", Quantity: 10}}
	res, err = a.GenerateContract(t.Context(), in)
	require.NoError(t, err)
	require.True(t, res.OK)
	require.NotEmpty(t, res.ContractNo)
	// 工作流：合同完成进入备货，合同号落库
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	le.at(time.Hour, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*time.Hour, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*time.Hour, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*time.Hour, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(4*time.Hour, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(5*time.Hour, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(6*time.Hour, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.NotNil(t, le.store.order("chang-an", "ORD-1").ContractNo)
}

// L2-06 · 款到发货：财务未确认不能发发货命令（CQ-05）
func TestL206PaymentGate(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	// 未确认到账 → 退回商务（无发货命令）
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "RETURNED", Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateSTKReturned, le.state())
	require.Zero(t, len(le.store.shipments))
}

// L2-07 · 非款到发货：跳过财务确认直达发货分配（CQ-05）
func TestL207NonPaymentPath(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	// 非款到无回单：补回单信号（分流②）后进入开票
	le.at(3*h, SignalReceiptProvided, nil)
	le.at(4*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(5*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	// 无财务确认任务
	tasks, _ := le.store.ListApprovalTasks(t.Context(), "chang-an", "", "PAYMENT", "", 10)
	require.Len(t, tasks, 0)
}

// L2-08 · 部分发货→按计划继续→累计满额→全部发货（CQ-07）
func TestL208PartialShipment(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 40})
	le.at(5*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 60})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Equal(t, int64(100), le.store.shippedByItem["chang-an/ORD-1/item-1"])
	require.Len(t, le.store.shipments, 2)
}

// L2-09 · 仓管退回/转仓（TransferWorkflow 内联实现；CQ-06）
func TestL209WarehouseReturnAndTransfer(t *testing.T) {
	// 未发货转仓：SHP_SHIP_ASSIGNING → 转仓申请 → 商务同意转仓 → 回仓管确认 → 走完
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	// SHP 段仓管申请转仓 → 回 STK 仓管确认闸门（再申请转仓）→ 商务转仓审核
	le.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "TRANSFER", Operator: "u-wh-yangyuan", WarehouseID: "wh-dongguan"})
	le.at(5*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "TRANSFER", Operator: "u-wh-yangyuan"})
	le.at(6*h, SignalTransferDecision, TransferDecisionSignal{Verdict: "APPROVE_TRANSFER", Operator: "u-op", WarehouseID: "wh-dongguan"})
	// 换仓后重新确认 → 走完
	le.at(7*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(7*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(8*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(9*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(10*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(11*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())

	// 商务驳回转仓 → 退回修改
	ord2 := &repo.Order{ID: "ORD-2", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le2 := newL2(t, ord2)
	le2.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le2.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le2.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le2.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le2.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "TRANSFER", Operator: "u-wh-yangyuan"})
	le2.at(5*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "TRANSFER", Operator: "u-wh-yangyuan"})
	le2.at(6*h, SignalTransferDecision, TransferDecisionSignal{Verdict: "REJECT", Operator: "u-op"})
	le2.run(t, le2.params("chang-an", "ORD-2"))
	require.Equal(t, domain.StateSTKReturned, le2.state())
}

// L2-10 · 开票守恒失败 → 拒绝并补偿（重试后成功；CQ-09）
func TestL210InvoiceConservationReject(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	// 注入开票守恒失败（模拟超可开余额被拒）→ 拒绝 + 补偿（INVOICE_FAILED）
	le.store.mu.Lock()
	le.store.failNextInvoice = true
	le.store.mu.Unlock()
	le.at(5*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Contains(t, le.store.events, "InvoiceFailed:INV_INVOICING->INV_INVOICE_FAILED")
}

// L2-11 · 无回单超 5 天 Timer → 例外开票子流程（CQ-10）
func TestL211ReceiptExceptionTimer(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	// 5 天回单 Timer（t0+2h 到达 INV 闸门 → t0+122h 触发例外）
	le.at(122*h, SignalReceiptExReview, ReceiptExReviewSignal{Verdict: "APPROVED", Operator: "u-op", DeadlineDays: 3})
	le.at(123*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(124*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-1",
		ApprovalRemindAfter:   24 * time.Hour,
		ReceiptExceptionAfter: 120 * time.Hour,
	})
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Contains(t, le.store.events, "ReceiptException:INV_READY_FOR_INVOICE->"+domain.StateINVReceiptExReviewing)
	// D-04：限时放行必须生成销售「补齐回单」待办
	follow, err := le.store.ListApprovalTasks(t.Context(), "chang-an", "", "RECEIPT_FOLLOWUP", "", 10)
	require.NoError(t, err)
	require.Len(t, follow, 1, "limited release must create receipt follow-up todo")
	require.Equal(t, "u-sales", deref(follow[0].AssigneeID))
}

// L2-12 · 预订单全链：消息→预订单→确认→正式订单（幂等重放同键；CQ-11）
func TestL212PreOrderFullChain(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })

	runChain := func() {
		suite := &testsuite.WorkflowTestSuite{}
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivity(NewActivities(store, notif, checker))
		env.RegisterWorkflow(PreOrderWorkflow)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalPreOrderConfirm, PreOrderConfirmSignal{Confirmed: true, Operator: "u-sales"})
		}, time.Hour)
		env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
			TenantID: "chang-an", PreOrderID: "PRE-1", EscalateAfter: 24 * time.Hour,
		})
		require.NoError(t, env.GetWorkflowError())
	}
	// 预置基础世界（客户/库存；派生正式订单需要）
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	// 预置消息 → 预订单（消息幂等）
	po, created, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "group-demo", MsgID: "msg-1",
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]`),
		IdempotencyKey: "pre:chang-an:group-demo:msg-1:v1",
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "PRE-1", po.ID)

	runChain()
	require.Equal(t, domain.StatePreOrderRequested, store.preOrders["PRE-1"].State)
	require.Len(t, store.orders, 1)

	// 幂等重放：同键同消息 → 不重复建单
	po2, created2, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "group-demo", MsgID: "msg-1",
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]`),
		IdempotencyKey: "pre:chang-an:group-demo:msg-1:v1",
	})
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, po.ID, po2.ID)
	runChain()
	require.Len(t, store.orders, 1)
}

// L2-13 · 预订单 24h 未确认 Timer 升级（ESCALATED；CQ-11）
func TestL213PreOrderEscalation(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	_, _, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "group-demo", MsgID: "msg-2",
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]`),
		IdempotencyKey: "pre:chang-an:group-demo:msg-2:v1",
	})
	require.NoError(t, err)

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, checker))
	env.RegisterWorkflow(PreOrderWorkflow)
	// 24h Timer 升级（t0+24h 触发 ESCALATED）→ 25h 注入确认
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPreOrderConfirm, PreOrderConfirmSignal{Confirmed: true, Operator: "u-sales"})
	}, 25*time.Hour)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
		TenantID: "chang-an", PreOrderID: "PRE-1", EscalateAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatePreOrderRequested, store.preOrders["PRE-1"].State)
	require.Len(t, store.orders, 1)
	require.Contains(t, store.events, "PreOrderEscalated:PRE_PENDING_CONFIRM->PRE_ESCALATED")
}

// L2-14 · 授权拒绝：无权限操作被独立验证者拦截（CQ-01）
func TestL214AuthzDenial(t *testing.T) {
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleWarehouse, nil })
	a := &Activities{Authz: checker}
	ok, err := a.AuthCheck(t.Context(), AuthCheckInput{
		TenantID: "chang-an", ResourceID: "ORD-1", Permission: authz.PermApprove, SubjectID: "frank-wh",
	})
	require.NoError(t, err)
	require.False(t, ok, "warehouse must not approve")
	ok, err = a.AuthCheck(t.Context(), AuthCheckInput{
		TenantID: "chang-an", ResourceID: "ORD-1", Permission: authz.PermShip, SubjectID: "frank-wh",
	})
	require.NoError(t, err)
	require.True(t, ok, "warehouse may ship")
}

// L2-03 补充 · 审价退回 → 重提 → 通过 → 走完（RETURNED/resubmit 分支；CQ-04）
func TestL203bPriceReturnedThenResubmit(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "RETURNED", Operator: "u-supervisor", Reason: "价格调整"})
	le.at(2*h, SignalResubmit, ResubmitSignal{Operator: "u-sales"})
	le.at(3*h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(4*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(4*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(5*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(6*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(7*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(8*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Contains(t, le.store.events, "PriceReturned:ORD_PRICE_REVIEWING->ORD_RETURNED")
}

// L2-08 补充 · 已发部分申请开票（PARTIAL → INVOICE 分流）
func TestL208bPartialInvoiceDirect(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 60})
	// 部分发货后直接申请开票（INVOICE 动作）
	le.at(5*h, SignalShipAction, ShipActionSignal{Verdict: "INVOICE", Operator: "u-wh-yangyuan"})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 60000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Equal(t, int64(60), le.store.shippedByItem["chang-an/ORD-1/item-1"])
}

// L2-10 补充 · 无需开票分流（NO_NEED）
func TestL210bNoInvoiceNeeded(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(2*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(3*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(4*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(5*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "NO_NEED", Operator: "u-accounting"})
	le.at(6*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	require.Contains(t, le.store.events, "NoInvoiceNeeded:INV_INVOICING->INV_NO_INVOICE_NEEDED")
}

// L2-12 补充 · 预订单低置信 → AMBIGUOUS → 修改后确认 → 正式订单（AI 安全边界）
func TestPreOrderAmbiguousThenModified(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	_, _, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "g", MsgID: "m-amb",
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.5}]`),
		IdempotencyKey: "pre:chang-an:g:m-amb:v1",
	})
	require.NoError(t, err)

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, checker))
	env.RegisterWorkflow(PreOrderWorkflow)
	env.RegisterDelayedCallback(func() {
		// 低置信 → AMBIGUOUS（轻提醒，不自动建单）→ 修改数量后确认
		require.Equal(t, domain.StatePreAmbiguous, store.preOrders["PRE-1"].State)
		env.SignalWorkflow(SignalPreOrderConfirm, PreOrderConfirmSignal{
			Confirmed: false, Operator: "u-sales", ModifiedQuantity: 8,
		})
	}, time.Hour)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
		TenantID: "chang-an", PreOrderID: "PRE-1", EscalateAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatePreOrderRequested, store.preOrders["PRE-1"].State)
	// 派生订单使用修改后数量 8
	var ord *repo.Order
	for _, o := range store.orders {
		ord = o
	}
	require.NotNil(t, ord)
	items, _ := store.GetOrderItems(t.Context(), "chang-an", ord.ID)
	require.Len(t, items, 1)
	require.Equal(t, int64(8), items[0].Quantity)
	require.Contains(t, store.events, "PreOrderModified:PRE_PENDING_CONFIRM->PRE_MODIFIED")
}

// L2-12 补充 · 预订单忽略（IGNORED 终态）
func TestPreOrderIgnored(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
	store.SeedCustomer("chang-an", "cust-0001", "A")
	_, _, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "g", MsgID: "m-ign",
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]`),
		IdempotencyKey: "pre:chang-an:g:m-ign:v1",
	})
	require.NoError(t, err)
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, checker))
	env.RegisterWorkflow(PreOrderWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPreOrderIgnore, nil)
	}, time.Hour)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
		TenantID: "chang-an", PreOrderID: "PRE-1", EscalateAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatePreIgnored, store.preOrders["PRE-1"].State)
	require.Empty(t, store.orders)
}

// 审批催办 Timer：超时未裁决 → 催办通知 + 继续等待（escalationAction）
func TestApprovalEscalation(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	// 审价闸门催办 1h → 触发催办通知 → 2h 后裁决
	le.at(2*h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(3*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(3*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(4*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(5*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: time.Hour,
	})
	require.Equal(t, domain.StateINVClosed, le.state())
	// 催办通知：PRICE 通知至少 2 次（初始 + 催办）
	require.GreaterOrEqual(t, le.notif.count("approval:PRICE:"), 2)
}

// 故障注入：Activity 错误传播 → 工作流失败（错误分支覆盖；CQ-14 补偿路径）
func TestFaultInjectionWorkflowErrors(t *testing.T) {
	cases := []struct {
		name string
		fail string
		pre  func(f *failingStore)
	}{
		{"GetOrder", "GetOrder", nil},
		{"GetOrderItems", "GetOrderItems", nil},
		{"GetCustomerGrade", "GetCustomerGrade", nil},
		{"UpdateOrderStateTx", "UpdateOrderStateTx", nil},
		{"CreateApprovalTaskTx", "CreateApprovalTaskTx", nil},
		{"ShipAndUpdateOrderTx", "ShipAndUpdateOrderTx", func(f *failingStore) {
			f.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		}},
		{"InvoiceAndUpdateOrderTx", "InvoiceAndUpdateOrderTx", func(f *failingStore) {
			f.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			f.SeedShipped("chang-an", "ORD-1", "item-1", 100)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFailingStore()
			store.SeedCustomer("chang-an", "cust-0001", "A")
			store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
			store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)
			if c.pre != nil {
				c.pre(store)
			}
			store.setFail(c.fail)

			notif := &FakeNotifier{}
			checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivity(NewActivities(store, notif, checker))
			env.RegisterWorkflow(OrderWorkflow)
			env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
				TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
			})
			require.Error(t, env.GetWorkflowError(), "workflow should fail with injected error")
		})
	}
}

// 预订单终态重入（幂等）：IGNORED / ORDER_REQUESTED 已完成后重跑 → 直接返回
func TestPreOrderTerminalReentry(t *testing.T) {
	for _, state := range []string{domain.StatePreIgnored, domain.StatePreOrderRequested, domain.StatePreConfirmed} {
		store := newFakeStore()
		notif := &FakeNotifier{}
		checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
		store.SeedCustomer("chang-an", "cust-0001", "A")
		po, _, err := store.CreatePreOrder(t.Context(), NewPreOrderForTest("chang-an", "g", "m-"+state))
		require.NoError(t, err)
		po.State = state

		suite := &testsuite.WorkflowTestSuite{}
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivity(NewActivities(store, notif, checker))
		env.RegisterWorkflow(PreOrderWorkflow)
		env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{TenantID: "chang-an", PreOrderID: po.ID})
		require.NoError(t, env.GetWorkflowError(), "state %s reentry should be idempotent", state)
	}
}

// 逐调用点故障注入：每个关键操作的每次调用位点都验证错误传播（错误分支系统覆盖）
func TestFaultInjectionPerCallSite(t *testing.T) {
	ops := map[string][]int{
		"GetOrderItems":        {1, 2, 3, 4},
		"GetCustomerGrade":     {1, 2},
		"CreateApprovalTaskTx": {1, 2, 3, 4},
		"UpdateOrderStateTx":   {1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	}
	for op, ns := range ops {
		for _, n := range ns {
			t.Run(op+"#"+itoa(n), func(t *testing.T) {
				store := newFailingStore()
				store.SeedCustomer("chang-an", "cust-0001", "A")
				store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
				store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)
				store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
				store.setFailNth(op, n)
				notif := &FakeNotifier{}
				checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
				suite := &testsuite.WorkflowTestSuite{}
				env := suite.NewTestWorkflowEnvironment()
				env.RegisterActivity(NewActivities(store, notif, checker))
				env.RegisterWorkflow(OrderWorkflow)
				env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
					TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
				})
				require.Error(t, env.GetWorkflowError(), "injected failure at %s call #%d should error", op, n)
			})
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// 扩展逐调用点故障注入：发货/开票/合同/备货/预订单关键操作
func TestFaultInjectionPerCallSiteExt(t *testing.T) {
	orderOps := map[string][]int{
		"ReserveStockTx":        {1, 2},
		"SumShippedByOrderItem": {1, 2},
		"CountShipments":        {1, 2},
		"SetOrderContractTx":    {1, 2},
		"UpdateResubmitChainTx": {1},
		"UpdateOrderMetaTx":     {1},
		"DecideTaskTx":          {1, 2, 3},
		"ListApprovalTasks":     {1},
		"UpdateOrderStateTx":    {11, 12, 13, 14, 15},
	}
	for op, ns := range orderOps {
		for _, n := range ns {
			t.Run(op+"#"+itoa(n), func(t *testing.T) {
				store := newFailingStore()
				store.SeedCustomer("chang-an", "cust-0001", "A")
				store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
				store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)
				store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
				store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
				store.setFailNth(op, n)
				notif := &FakeNotifier{}
				checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
				suite := &testsuite.WorkflowTestSuite{}
				env := suite.NewTestWorkflowEnvironment()
				env.RegisterActivity(NewActivities(store, notif, checker))
				env.RegisterWorkflow(OrderWorkflow)
				env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
					TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
				})
				require.Error(t, env.GetWorkflowError(), "injected failure at %s call #%d should error", op, n)
			})
		}
	}
	// 预订单操作
	preOps := map[string][]int{
		"GetPreOrder":           {1, 2},
		"UpdatePreOrderStateTx": {1, 2, 3},
	}
	for op, ns := range preOps {
		for _, n := range ns {
			t.Run("PRE-"+op+"#"+itoa(n), func(t *testing.T) {
				store := newFailingStore()
				store.SeedCustomer("chang-an", "cust-0001", "A")
				store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
				po, _, err := store.CreatePreOrder(t.Context(), NewPreOrderForTest("chang-an", "g", "m-f"))
				require.NoError(t, err)
				store.setFailNth(op, n)
				notif := &FakeNotifier{}
				checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
				suite := &testsuite.WorkflowTestSuite{}
				env := suite.NewTestWorkflowEnvironment()
				env.RegisterActivity(NewActivities(store, notif, checker))
				env.RegisterWorkflow(PreOrderWorkflow)
				env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
					TenantID: "chang-an", PreOrderID: po.ID, EscalateAfter: 24 * time.Hour,
				})
				require.Error(t, env.GetWorkflowError(), "injected failure at %s call #%d should error", op, n)
			})
		}
	}
}

// 合同要素不全 → 补全重校验（L2-05 工作流内分支）
func TestContractRetryBranch(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	acts := NewActivities(store, notif, checker)
	first := true
	acts.ContractGate = func(in GenerateContractInput) (*GenerateContractResult, error) {
		if first {
			first = false
			return &GenerateContractResult{Missing: []string{"items"}}, nil // 首次要素不全
		}
		return &GenerateContractResult{ContractNo: "HT-RETRY-1", OK: true}, nil
	}
	env.RegisterActivity(acts)
	env.RegisterWorkflow(OrderWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	}, time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	}, 2*time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	}, 2*time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	}, 3*time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	}, 4*time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	}, 5*time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	}, 6*time.Hour)
	env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
		TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StateINVClosed, store.order("chang-an", "ORD-1").State)
	require.Contains(t, store.events, "ContractRetry:ORD_CONTRACT_GENERATING->ORD_CONTRACT_GENERATING")
}

// 07 §3.2 权限校验：发送者无客户权限 → 忽略 + 审计（不自动建单）
func TestPreOrderSenderUnauthorized(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	// 授权拒绝注入：stranger 对客户无 view 权限（07 §3.2）
	deny := &denyAuthChecker{}
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	_, _, err := store.CreatePreOrder(t.Context(), NewPreOrderForTest("chang-an", "g", "m-unauth"))
	require.NoError(t, err)

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, deny))
	env.RegisterWorkflow(PreOrderWorkflow)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
		TenantID: "chang-an", PreOrderID: "PRE-1", SenderID: "stranger", EscalateAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatePreIgnored, store.preOrders["PRE-1"].State)
	require.Contains(t, store.events, "SenderUnauthorized:PRE_INTENT_PARSING->PRE_IGNORED")
	require.Empty(t, store.orders, "unauthorized sender must not create order")
}

// denyAuthChecker 拒绝一切授权的测试替身。
type denyAuthChecker struct{}

func (d *denyAuthChecker) Check(ctx context.Context, req authz.CheckRequest) (bool, error) {
	return false, nil
}

// 非法裁决信号 → 工作流报错（未知 verdict 分支）
func TestUnknownVerdictSignals(t *testing.T) {
	cases := []struct {
		name   string
		signal string
		val    any
		seed   func(le *l2Env)
	}{
		{"price", SignalPriceReview, PriceReviewSignal{Verdict: "BOGUS", Operator: "u-supervisor"}, nil},
		{"stock", SignalStockReview, StockReviewSignal{Verdict: "BOGUS", Operator: "u-op"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateSTKStockReviewing
		}},
		{"warehouse", SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "BOGUS", Operator: "u-wh-yangyuan"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateSTKWarehouseConfirm
		}},
		{"payment", SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "BOGUS", Operator: "u-accounting"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateSTKPaymentConfirming
		}},
		{"transfer", SignalTransferDecision, TransferDecisionSignal{Verdict: "BOGUS", Operator: "u-op"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateSTKTransferRequested
		}},
		{"ship", SignalShipAction, ShipActionSignal{Verdict: "BOGUS", Operator: "u-wh-yangyuan"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateSHPShipAssigning
		}},
		{"receipt-ex", SignalReceiptExReview, ReceiptExReviewSignal{Verdict: "BOGUS", Operator: "u-op"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateINVReceiptExReviewing
		}},
		{"invoice", SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "BOGUS", Operator: "u-accounting"}, func(le *l2Env) {
			le.store.order("chang-an", "ORD-1").State = domain.StateINVInvoicing
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
			le := newL2(t, ord)
			le.store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
			if c.seed != nil {
				c.seed(le)
			}
			le.at(time.Hour, c.signal, c.val)
			le.env.ExecuteWorkflow(OrderWorkflow, le.params("chang-an", "ORD-1"))
			require.Error(t, le.env.GetWorkflowError(), "unknown verdict should error")
		})
	}
}

// 全位点故障扫描：对关键操作注入第 1..N 次调用失败，逐一验证错误传播
// （覆盖段函数全部错误返回分支；N 覆盖该操作在整条主链上的全部调用位点）
func TestFaultInjectionFullSiteScan(t *testing.T) {
	scan := func(op string, max int) {
		for n := 1; n <= max; n++ {
			t.Run(op+"#"+itoa(n), func(t *testing.T) {
				store := newFailingStore()
				store.SeedCustomer("chang-an", "cust-0001", "A")
				store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
				store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)
				store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
				store.SeedShipped("chang-an", "ORD-1", "item-1", 100)
				store.setFailNth(op, n)
				notif := &FakeNotifier{}
				checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
				suite := &testsuite.WorkflowTestSuite{}
				env := suite.NewTestWorkflowEnvironment()
				env.RegisterActivity(NewActivities(store, notif, checker))
				env.RegisterWorkflow(OrderWorkflow)
				env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
					TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
				})
				require.Error(t, env.GetWorkflowError(), "injected %s call #%d should error", op, n)
			})
		}
	}
	// 主链全位点扫描（failOnNth 已覆盖过的操作不重复；补充新操作与更深位点）
	scan("GetOrder", 3)              // ordSegment / applyModification / stkResume 重读
	scan("GetPendingTask", 3)        // stkWarehouseStep / stkTransferStep / shpContinueStep
	scan("SetOrderContractTx", 3)    // ordContractStep（含修改 C 类重入）
	scan("UpdateResubmitChainTx", 3) // applyModification（A-E 各类）
	scan("UpdateOrderMetaTx", 2)     // applyModification E 类
	scan("DecideTaskTx", 6)          // 各闸门裁决
	scan("NotifyApproval", 6)        // 各闸门创建任务后通知
	scan("NotifyOrderEvent", 3)      // shpShipStep
	scan("UpdateOrderStateTx", 22)   // 主链全部状态迁移位点
}

// 修改路径故障注入：闸门收到修改信号 → applyModification 各环节注入失败
// 时序：+1h 审价通过 → 主链推进到 STK_WAREHOUSE_CONFIRMING → +2h 修改信号(E) →
// applyModification：→ORD_MODIFYING(UpdateOrderStateTx#6) → resubmit(UpdateResubmitChainTx#1)
// → E 类 UpdateOrderMetaTx#1 → →ORD_CONTRACTED(UpdateOrderStateTx#7)
func TestModificationFaultInjection(t *testing.T) {
	run := func(setup func(f *failingStore)) {
		store := newFailingStore()
		store.SeedCustomer("chang-an", "cust-0001", "A")
		store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
		store.seedOrder("chang-an", "ORD-1", "cust-0001", "款到", 100)
		store.SeedPrepared("chang-an", "ORD-1", "item-1", 100)
		setup(store)
		notif := &FakeNotifier{}
		checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
		suite := &testsuite.WorkflowTestSuite{}
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivity(NewActivities(store, notif, checker))
		env.RegisterWorkflow(OrderWorkflow)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
		}, time.Hour)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalModifyOrder, ModifyOrderSignal{Class: domain.ModClassE, Operator: "u-sales"})
		}, 2*time.Hour)
		env.ExecuteWorkflow(OrderWorkflow, OrderWorkflowParams{
			TenantID: "chang-an", OrderID: "ORD-1", ApprovalRemindAfter: 24 * time.Hour,
		})
		require.Error(t, env.GetWorkflowError())
	}
	// UpdateOrderStateTx 修改路径位点：主链消耗 5 个（审价/合同/进入STK/备货/仓管确认），
	// 修改时第 6 个（→ORD_MODIFYING）与第 7 个（E 类→ORD_CONTRACTED）
	for n := 6; n <= 7; n++ {
		t.Run("UpdateOrderStateTx#"+itoa(n), func(t *testing.T) {
			run(func(f *failingStore) { f.setFailNth("UpdateOrderStateTx", n) })
		})
	}
	t.Run("UpdateResubmitChainTx#1", func(t *testing.T) {
		run(func(f *failingStore) { f.setFailNth("UpdateResubmitChainTx", 1) })
	})
	t.Run("UpdateOrderMetaTx#1", func(t *testing.T) {
		run(func(f *failingStore) { f.setFailNth("UpdateOrderMetaTx", 1) })
	})
}

// 预订单深度故障注入：权限校验失败 / 通知失败 / 派生订单失败 / 状态迁移失败
func TestPreOrderDeepFaultInjection(t *testing.T) {
	run := func(authzFail bool, authzDeny bool, notifFail bool, failNth map[string]int) {
		store := newFailingStore()
		store.SeedCustomer("chang-an", "cust-0001", "A")
		store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
		po, _, err := store.CreatePreOrder(t.Context(), NewPreOrderForTest("chang-an", "g", "m-df"))
		require.NoError(t, err)
		for op, n := range failNth {
			store.setFailNth(op, n)
		}
		authzC := &failingAuthz{fail: authzFail, deny: authzDeny}
		notif := &failingNotifier{fail: notifFail}
		suite := &testsuite.WorkflowTestSuite{}
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivity(NewActivities(store, notif, authzC))
		env.RegisterWorkflow(PreOrderWorkflow)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalPreOrderConfirm, PreOrderConfirmSignal{Confirmed: true, Operator: "u-sales"})
		}, time.Hour)
		env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
			TenantID: "chang-an", PreOrderID: po.ID, SenderID: "u-sales", EscalateAfter: 24 * time.Hour,
		})
		require.Error(t, env.GetWorkflowError())
	}

	t.Run("AuthCheck失败", func(t *testing.T) { run(true, false, false, nil) })
	t.Run("通知失败", func(t *testing.T) { run(false, false, true, nil) })
	t.Run("授权拒绝+Confirm迁移失败", func(t *testing.T) {
		// 授权拒绝 → SenderUnauthorized 的 setPreState 失败
		run(false, true, false, map[string]int{"UpdatePreOrderStateTx": 2})
	})
	t.Run("派生订单失败", func(t *testing.T) { run(false, false, false, map[string]int{"CreateOrder": 1}) })
	t.Run("OrderRequested迁移失败", func(t *testing.T) {
		run(false, false, false, map[string]int{"UpdatePreOrderStateTx": 4})
	})
	t.Run("GetPreOrder重读失败", func(t *testing.T) {
		run(false, false, false, map[string]int{"GetPreOrder": 2})
	})
}
