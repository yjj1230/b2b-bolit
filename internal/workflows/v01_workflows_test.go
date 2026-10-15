package workflows

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── 0.1 发布版工作流测试（docs/v0.1/09-testing N-03/N-04）──

// N-03 · 商务备货选仓 + 收货地址（A1）：选备货仓生效 → 备货单落到所选仓库；
// 收货地址随事件留痕；锁库存暂缓断言：备货不触碰库存（M1）。
func TestN03StockReviewWarehouseSelection(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	// 订单明细带收货地址（A1）
	le.store.setOrderItem(ord.TenantID, ord.ID, "item-1", "prod-c68", "wh-yangyuan", 100, 1000, "湖南岳阳 · 云溪区")
	le.store.setInventory(ord.TenantID, "wh-dongguan", "prod-c68", 500)

	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	// 商务按收货地址选仓：东莞仓（非默认阳原仓）
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-dongguan", Note: "按湖南岳阳收货地址选仓"})
	le.at(3*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(4*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(5*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(6*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(7*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())

	// 选仓生效：备货单落在所选仓库
	sp := le.store.StockPrepareFor("chang-an", "STK-"+shortID("ORD-1:1"))
	require.NotNil(t, sp, "stock prepare created")
	require.Equal(t, "wh-dongguan", sp.WarehouseID, "选仓生效")
	// 发货单继承所选仓库（N-03 断言）
	for _, shp := range le.store.Shipments() {
		require.Equal(t, "wh-dongguan", shp.WarehouseID, "shipment inherits selected warehouse")
	}

	// 锁库存暂缓（M1/A1）：备货不扣库存（阳原/东莞均不变）
	require.Equal(t, int64(1000), le.store.InventoryOf("chang-an", "wh-yangyuan", "prod-c68"))
	require.Equal(t, int64(500), le.store.InventoryOf("chang-an", "wh-dongguan", "prod-c68"))
}

// N-03b · 备货驳回（N4 四按钮的「驳回」）→ 通知销售 → 重提后走完。
func TestN03bStockReviewReturnedThenApprove(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "RETURNED", Operator: "u-op", Note: "缺货，请商务核实"})
	le.at(3*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: "wh-yangyuan"})
	le.at(4*h, SignalWarehouseConfirm, WarehouseConfirmSignal{Verdict: "APPROVED", Operator: "u-wh-yangyuan"})
	le.at(5*h, SignalPaymentConfirm, PaymentConfirmSignal{Verdict: "APPROVED", Operator: "u-accounting"})
	le.at(6*h, SignalShipAction, ShipActionSignal{Verdict: "SHIP", Operator: "u-wh-yangyuan", Qty: 100})
	le.at(7*h, SignalInvoiceIssued, InvoiceIssuedSignal{Verdict: "ISSUED", Operator: "u-accounting"})
	le.at(8*h, SignalPaymentTracked, PaymentTrackedSignal{Amount: 100000, Operator: "u-accounting"})
	le.run(t, le.params("chang-an", "ORD-1"))
	require.Equal(t, domain.StateINVClosed, le.state())
	// 驳回事件留痕 + 销售通知
	require.True(t, containsPrefix(le.store.events, "StockReturned:"), "stock returned event missing")
	require.GreaterOrEqual(t, le.notif.count("order-event:"), 1, "销售被通知")
}

// N-03c · 选仓校验：APPROVED 未选仓 → 工作流报错（非法输入拦截）。
func TestN03cStockReviewMissingWarehouse(t *testing.T) {
	ord := &repo.Order{ID: "ORD-1", TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到"}
	le := newL2(t, ord)
	h := time.Hour
	le.at(h, SignalPriceReview, PriceReviewSignal{Verdict: "APPROVED", Operator: "u-supervisor"})
	le.at(2*h, SignalStockReview, StockReviewSignal{Verdict: "APPROVED", Operator: "u-op", WarehouseID: ""})
	le.env.ExecuteWorkflow(OrderWorkflow, le.params("chang-an", "ORD-1"))
	require.Error(t, le.env.GetWorkflowError(), "approve without warehouse must fail")
}

// N-04 · 客户群归属锁定（A7）：群上下文带出的客户随候选锁定，
// 确认派生订单落到该客户名下（不再硬编码演示客户）。
func TestN04PreOrderCustomerFromGroupContext(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedCustomer("chang-an", "cust-0002", "B")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
	cands := []IntentCandidate{{
		ProductID: "prod-c68", Quantity: 50, UnitPriceCents: 3200, Confidence: 0.95,
		CustomerID: "cust-0002", // 群上下文带出（XSZL2 入口锁定）
	}}
	candsJSON, err := json.Marshal(cands)
	require.NoError(t, err)
	po, created, err := store.CreatePreOrder(t.Context(), repo.NewPreOrder{
		TenantID: "chang-an", GroupID: "group-demo", MsgID: "m-n04",
		Candidates: candsJSON, IdempotencyKey: "pre:chang-an:group-demo:m-n04:v1",
	})
	require.NoError(t, err)
	require.True(t, created)

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, checker))
	env.RegisterWorkflow(PreOrderWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPreOrderConfirm, PreOrderConfirmSignal{Confirmed: true, Operator: "u-sales"})
	}, time.Hour)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{
		TenantID: "chang-an", PreOrderID: po.ID, SenderID: "u-sales", EscalateAfter: 24 * time.Hour,
	})
	require.NoError(t, env.GetWorkflowError())
	// 派生订单归属群上下文客户（N-04：归属锁定）
	ords := store.Orders()
	require.Len(t, ords, 1)
	require.Equal(t, "cust-0002", ords[0].CustomerID, "derived order must belong to group-context customer")
}
