package workflows

import (
	"context"
	"fmt"
	"sync"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/repo"
)

// ── failingStore：故障注入包装（覆盖 Activity 错误传播分支；docs/03-testing.html §5 故障钩子）──

// failingStore 在指定操作上返回注入错误。
type failingStore struct {
	FakeStore
	mu        sync.Mutex
	failOn    map[string]bool // 操作名 → 注入失败
	failAll   bool
	callCount map[string]int // 操作名 → 已调用次数
	failOnNth map[string]int // 操作名 → 第 N 次调用时失败（逐调用点覆盖）
}

func newFailingStore() *failingStore {
	return &failingStore{
		FakeStore: *newFakeStore(), failOn: map[string]bool{},
		callCount: map[string]int{}, failOnNth: map[string]int{},
	}
}

func (f *failingStore) shouldFail(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount[op]++
	if f.failAll || f.failOn[op] {
		return true
	}
	return f.failOnNth[op] == f.callCount[op]
}

func (f *failingStore) setFailNth(op string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnNth[op] = n
}

func (f *failingStore) fail(op string) error { return fmt.Errorf("injected failure: %s", op) }

func (f *failingStore) setFail(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[op] = true
}

// ── 覆写关键操作 ──

func (f *failingStore) GetOrder(ctx context.Context, tenantID, orderID string) (*repo.Order, error) {
	if f.shouldFail("GetOrder") {
		return nil, f.fail("GetOrder")
	}
	return f.FakeStore.GetOrder(ctx, tenantID, orderID)
}

func (f *failingStore) GetOrderItems(ctx context.Context, tenantID, orderID string) ([]repo.OrderItem, error) {
	if f.shouldFail("GetOrderItems") {
		return nil, f.fail("GetOrderItems")
	}
	return f.FakeStore.GetOrderItems(ctx, tenantID, orderID)
}

func (f *failingStore) GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error) {
	if f.shouldFail("GetCustomerGrade") {
		return "", f.fail("GetCustomerGrade")
	}
	return f.FakeStore.GetCustomerGrade(ctx, tenantID, customerID)
}

func (f *failingStore) UpdateOrderStateTx(ctx context.Context, tenantID, orderID, fromState, toState, action, operator, reason string, detail []byte) error {
	if f.shouldFail("UpdateOrderStateTx") {
		return f.fail("UpdateOrderStateTx")
	}
	return f.FakeStore.UpdateOrderStateTx(ctx, tenantID, orderID, fromState, toState, action, operator, reason, detail)
}

func (f *failingStore) CreateApprovalTaskTx(ctx context.Context, in repo.NewApprovalTask) (string, error) {
	if f.shouldFail("CreateApprovalTaskTx") {
		return "", f.fail("CreateApprovalTaskTx")
	}
	return f.FakeStore.CreateApprovalTaskTx(ctx, in)
}

func (f *failingStore) ShipAndUpdateOrderTx(ctx context.Context, ship repo.Shipment, items []repo.ShipmentItem, fromState, toState, operator, reason string, detail []byte) error {
	if f.shouldFail("ShipAndUpdateOrderTx") {
		return f.fail("ShipAndUpdateOrderTx")
	}
	return f.FakeStore.ShipAndUpdateOrderTx(ctx, ship, items, fromState, toState, operator, reason, detail)
}

func (f *failingStore) InvoiceAndUpdateOrderTx(ctx context.Context, inv repo.Invoice, items []repo.InvoiceItem, fromState, toState, operator, reason string, detail []byte) error {
	if f.shouldFail("InvoiceAndUpdateOrderTx") {
		return f.fail("InvoiceAndUpdateOrderTx")
	}
	return f.FakeStore.InvoiceAndUpdateOrderTx(ctx, inv, items, fromState, toState, operator, reason, detail)
}

func (f *failingStore) UpdatePreOrderStateTx(ctx context.Context, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error {
	if f.shouldFail("UpdatePreOrderStateTx") {
		return f.fail("UpdatePreOrderStateTx")
	}
	return f.FakeStore.UpdatePreOrderStateTx(ctx, tenantID, preOrderID, fromState, toState, action, reason, detail, operator)
}

func (f *failingStore) CreateOrder(ctx context.Context, in repo.NewOrder) (*repo.Order, bool, error) {
	if f.shouldFail("CreateOrder") {
		return nil, false, f.fail("CreateOrder")
	}
	return f.FakeStore.CreateOrder(ctx, in)
}

func (f *failingStore) GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*repo.PreOrder, error) {
	if f.shouldFail("GetPreOrder") {
		return nil, f.fail("GetPreOrder")
	}
	return f.FakeStore.GetPreOrder(ctx, tenantID, preOrderID)
}

func (f *failingStore) ReserveStockTx(ctx context.Context, sp repo.StockPrepare, items []repo.StockPrepareItem, fromState, toState, operator, reason string, detail []byte) error {
	if f.shouldFail("ReserveStockTx") {
		return f.fail("ReserveStockTx")
	}
	return f.FakeStore.ReserveStockTx(ctx, sp, items, fromState, toState, operator, reason, detail)
}

func (f *failingStore) SumShippedByOrderItem(ctx context.Context, tenantID, orderID string) (map[string]int64, error) {
	if f.shouldFail("SumShippedByOrderItem") {
		return nil, f.fail("SumShippedByOrderItem")
	}
	return f.FakeStore.SumShippedByOrderItem(ctx, tenantID, orderID)
}

func (f *failingStore) CountShipments(ctx context.Context, tenantID, orderID string) (int, error) {
	if f.shouldFail("CountShipments") {
		return 0, f.fail("CountShipments")
	}
	return f.FakeStore.CountShipments(ctx, tenantID, orderID)
}

func (f *failingStore) SetOrderContractTx(ctx context.Context, tenantID, orderID, contractNo string) error {
	if f.shouldFail("SetOrderContractTx") {
		return f.fail("SetOrderContractTx")
	}
	return f.FakeStore.SetOrderContractTx(ctx, tenantID, orderID, contractNo)
}

func (f *failingStore) UpdateOrderMetaTx(ctx context.Context, tenantID, orderID, contractNo string, payload []byte) error {
	if f.shouldFail("UpdateOrderMetaTx") {
		return f.fail("UpdateOrderMetaTx")
	}
	return f.FakeStore.UpdateOrderMetaTx(ctx, tenantID, orderID, contractNo, payload)
}

func (f *failingStore) UpdateResubmitChainTx(ctx context.Context, tenantID, orderID string, revision int, fromOrderID string) error {
	if f.shouldFail("UpdateResubmitChainTx") {
		return f.fail("UpdateResubmitChainTx")
	}
	return f.FakeStore.UpdateResubmitChainTx(ctx, tenantID, orderID, revision, fromOrderID)
}

func (f *failingStore) DecideTaskTx(ctx context.Context, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error {
	if f.shouldFail("DecideTaskTx") {
		return f.fail("DecideTaskTx")
	}
	return f.FakeStore.DecideTaskTx(ctx, tenantID, taskID, toState, verdict, decidedBy, detail)
}

func (f *failingStore) ListApprovalTasks(ctx context.Context, tenantID string, assignee, kind, state string, limit int) ([]repo.ApprovalTask, error) {
	if f.shouldFail("ListApprovalTasks") {
		return nil, f.fail("ListApprovalTasks")
	}
	return f.FakeStore.ListApprovalTasks(ctx, tenantID, assignee, kind, state, limit)
}

// failingAuthz 授权判定按需失败/拒绝（07 §3.2 权限校验路径）。
type failingAuthz struct {
	fail bool
	deny bool
}

func (f *failingAuthz) Check(ctx context.Context, req authz.CheckRequest) (bool, error) {
	if f.fail {
		return false, fmt.Errorf("inject: authz check failed")
	}
	if f.deny {
		return false, nil
	}
	return true, nil
}

// failingNotifier 出站通知按需失败。
type failingNotifier struct {
	FakeNotifier
	fail bool
}

func (f *failingNotifier) NotifyPreOrderConfirm(ctx context.Context, tenantID, preOrderID string, candidates []byte) error {
	if f.fail {
		return fmt.Errorf("inject: notify failed")
	}
	return f.FakeNotifier.NotifyPreOrderConfirm(ctx, tenantID, preOrderID, candidates)
}
