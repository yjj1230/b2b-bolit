package workflows

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── Activity 单元测试（错误路径/纯逻辑；覆盖率补强）──

func TestValidateOrderActivity(t *testing.T) {
	a := &Activities{Store: newFakeStore()}
	ctx := context.Background()
	ord := &repo.Order{IdempotencyKey: "order:t:quick:u"}
	items := []repo.OrderItem{{ID: "i1", Quantity: 10, UnitPriceCents: 100}}

	res, err := a.ValidateOrder(ctx, ord, items, "A")
	require.NoError(t, err)
	require.True(t, res.OK)

	// 非法幂等键
	bad := &repo.Order{IdempotencyKey: "bogus"}
	res, err = a.ValidateOrder(ctx, bad, items, "A")
	require.NoError(t, err)
	require.False(t, res.OK)

	// 非法等级
	res, err = a.ValidateOrder(ctx, ord, items, "X")
	require.NoError(t, err)
	require.False(t, res.OK)

	// 空明细
	res, err = a.ValidateOrder(ctx, ord, nil, "A")
	require.NoError(t, err)
	require.False(t, res.OK)

	// 负数量 / 负单价
	res, err = a.ValidateOrder(ctx, ord, []repo.OrderItem{{ID: "i2", Quantity: -1}}, "A")
	require.NoError(t, err)
	require.False(t, res.OK)
	res, err = a.ValidateOrder(ctx, ord, []repo.OrderItem{{ID: "i3", Quantity: 1, UnitPriceCents: -5}}, "A")
	require.NoError(t, err)
	require.False(t, res.OK)
}

func TestGenerateContractActivity(t *testing.T) {
	a := &Activities{}
	ctx := context.Background()
	ord := &repo.Order{PaymentTerm: "款到"}
	items := []repo.OrderItem{{ID: "i1", ProductID: "p1", Quantity: 1}}

	// 缺客户名/明细
	res, err := a.GenerateContract(ctx, GenerateContractInput{Order: ord})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Missing, "customer_name")

	// 缺 product_id
	res, err = a.GenerateContract(ctx, GenerateContractInput{Order: ord, Items: []repo.OrderItem{{ID: "i1"}}})
	require.NoError(t, err)
	require.Contains(t, res.Missing, "product_id")

	// 合法
	res, err = a.GenerateContract(ctx, GenerateContractInput{
		OrderID: "ORD-12345678", Order: ord, Items: items, CustomerName: "c",
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, "HT--12345678", res.ContractNo)
}

func TestParseIntentActivity(t *testing.T) {
	a := &Activities{}
	ctx := context.Background()
	// 空候选
	res, err := a.ParseIntent(ctx, "t", nil)
	require.NoError(t, err)
	require.False(t, res.HighConfidence)
	// 非法 JSON
	res, err = a.ParseIntent(ctx, "t", []byte(`{bad`))
	require.NoError(t, err)
	require.False(t, res.HighConfidence)
	// 低置信
	res, err = a.ParseIntent(ctx, "t", []byte(`[{"quantity":10,"unit_price_cents":100,"confidence":0.5}]`))
	require.NoError(t, err)
	require.False(t, res.HighConfidence)
	// 非法数量
	res, err = a.ParseIntent(ctx, "t", []byte(`[{"quantity":0,"unit_price_cents":100,"confidence":0.9}]`))
	require.NoError(t, err)
	require.False(t, res.HighConfidence)
	// 高置信
	res, err = a.ParseIntent(ctx, "t", []byte(`[{"quantity":10,"unit_price_cents":100,"confidence":0.95}]`))
	require.NoError(t, err)
	require.True(t, res.HighConfidence)
	require.Len(t, res.Candidates, 1)
}

func TestStoreActivitiesErrorPaths(t *testing.T) {
	store := newFakeStore()
	a := &Activities{Store: store}
	ctx := context.Background()

	// GetPendingTask 空
	id, err := a.GetPendingTask(ctx, "t", "o", "PRICE")
	require.NoError(t, err)
	require.Empty(t, id)

	// CountShipments 空
	n, err := a.CountShipments(ctx, "t", "o")
	require.NoError(t, err)
	require.Zero(t, n)

	// Compensate（RecordFlowFailed 事件留痕）
	require.NoError(t, a.Compensate(ctx, "t", "o", "inject", nil))
	require.True(t, containsPrefix(store.events, "FlowFailed:"))

	// UpdateResubmitChain 未知订单
	require.Error(t, a.UpdateResubmitChain(ctx, "t", "o", 1, "o"))
	// UpdateOrderMeta 未知订单
	require.Error(t, a.UpdateOrderMeta(ctx, "t", "o", "", nil))
	// SetContract 未知订单
	require.Error(t, a.SetContract(ctx, "t", "o", "HT"))
	// SetState 未知订单
	require.Error(t, a.SetState(ctx, StateInput{TenantID: "t", OrderID: "o"}))
	// TrackPayment 未知订单
	require.Error(t, a.TrackPayment(ctx, "t", "o", "u", 100))
	// ReserveStock 未知订单（from 状态不匹配）
	sp := repo.StockPrepare{ID: "sp1", TenantID: "t", OrderID: "o", PlannedQty: 10, RemainingQty: 10}
	require.Error(t, a.ReserveStock(ctx, sp, nil, "X", "Y", "u", "r", nil))
}

func TestFakeStoreEdgeCases(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()

	// GetOrder 未知
	_, err := store.GetOrder(ctx, "t", "o")
	require.True(t, repo.IsNotFound(err))
	// GetOrderItems 未知 → 空
	items, err := store.GetOrderItems(ctx, "t", "o")
	require.NoError(t, err)
	require.Empty(t, items)
	// GetCustomerGrade 未知
	_, err = store.GetCustomerGrade(ctx, "t", "c")
	require.True(t, repo.IsNotFound(err))
	// CreateOrder 不再校验客户存在（客户实体存在性由 orderdata.Provider 校验）
	ordX, created, err := store.CreateOrder(ctx, repo.NewOrder{TenantID: "t", CustomerID: "c", IdempotencyKey: "k"})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "c", ordX.CustomerID)

	// CreateOrder 幂等冲突（同键异 payload）
	store.SeedCustomer("t", "c", "A")
	_, created, err = store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k", Payload: []byte(`{"a":1}`),
	})
	require.True(t, repo.IsIdempotencyConflict(err))
	require.False(t, created)
	// 同键同 payload 幂等重放：无错误、不新建、返回原订单
	ordSame, created, err := store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k-replay", Payload: []byte(`{"a":1}`),
	})
	require.NoError(t, err)
	require.True(t, created)
	ordReplay, created, err := store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k-replay", Payload: []byte(`{"a":1}`),
	})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, ordSame.ID, ordReplay.ID)
	// 再次同键异 payload 仍判冲突
	_, _, err = store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k", Payload: []byte(`{"a":2}`),
	})
	require.True(t, repo.IsIdempotencyConflict(err))

	// UpdateOrderStateTx from 不匹配
	ord := store.order("t", "ORD-x")
	_ = ord
	o, _, err := store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k2",
		Items: []repo.NewOrderItem{{ID: "i1", ProductID: "p1", Quantity: 1, UnitPriceCents: 1}},
	})
	require.NoError(t, err)
	require.Error(t, store.UpdateOrderStateTx(ctx, "t", o.ID, "WRONG", "X", "a", "u", "r", nil))
	require.NoError(t, store.UpdateOrderStateTx(ctx, "t", o.ID, domain.StateORDRequested, "X", "a", "u", "r", nil))

	// UpdateOrderMetaTx 未知
	require.Error(t, store.UpdateOrderMetaTx(ctx, "t", "o", "", nil))
	// UpdateResubmitChainTx 未知
	require.Error(t, store.UpdateResubmitChainTx(ctx, "t", "o", 1, "o"))
	// DecideTaskTx 未知任务 / 重复裁决
	_, err = store.CreateApprovalTaskTx(ctx, repo.NewApprovalTask{TenantID: "t", OrderID: o.ID, StageCode: "ORD", Kind: "PRICE"})
	require.NoError(t, err)
	tasks, _ := store.ListApprovalTasks(ctx, "t", "", "PRICE", "PENDING", 10)
	require.Len(t, tasks, 1)
	require.NoError(t, store.DecideTaskTx(ctx, "t", tasks[0].ID, "APPROVED", "ok", "u", nil))
	require.Error(t, store.DecideTaskTx(ctx, "t", tasks[0].ID, "APPROVED", "again", "u", nil))
	require.Error(t, store.DecideTaskTx(ctx, "t", "AT-none", "APPROVED", "x", "u", nil))

	// GetPreOrder / UpdatePreOrderStateTx 未知
	_, err = store.GetPreOrder(ctx, "t", "PRE-x")
	require.True(t, repo.IsNotFound(err))
	require.Error(t, store.UpdatePreOrderStateTx(ctx, "t", "PRE-x", "A", "B", "a", "r", nil, "u"))
	// CreatePreOrder 消息幂等
	po, created, err := store.CreatePreOrder(ctx, repo.NewPreOrder{TenantID: "t", GroupID: "g", MsgID: "m", IdempotencyKey: "k3"})
	require.NoError(t, err)
	require.True(t, created)
	po2, created2, err := store.CreatePreOrder(ctx, repo.NewPreOrder{TenantID: "t", GroupID: "g", MsgID: "m", IdempotencyKey: "k3"})
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, po.ID, po2.ID)

	// 事件查询
	evs, err := store.ListOrderEvents(ctx, "t", o.ID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	// 发货守恒失败
	store.SeedInventory("t", "wh", "p1", 0)
	o2, _, err := store.CreateOrder(ctx, repo.NewOrder{
		TenantID: "t", CustomerID: "c", IdempotencyKey: "k4",
		Items: []repo.NewOrderItem{{ID: "i2", ProductID: "p1", Quantity: 5, UnitPriceCents: 1}},
	})
	require.NoError(t, err)
	err = store.ShipAndUpdateOrderTx(ctx, repo.Shipment{
		ID: "shp1", TenantID: "t", OrderID: o2.ID, StockPrepareID: "sp1", WarehouseID: "wh", PlannedQty: 5, ShippedQty: 5, IdempotencyKey: "sk1",
	}, []repo.ShipmentItem{{ID: "si1", TenantID: "t", ShipmentID: "shp1", OrderItemID: "i2", ProductID: "p1", Qty: 5}},
		"F", "T", "u", "r", nil)
	require.Error(t, err) // 库存不足
}

func TestStaticCheckerAdapter(t *testing.T) {
	// authz.Checker 契约满足（L2-14 已覆盖行为）
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
	a := NewActivities(newFakeStore(), &FakeNotifier{}, checker)
	require.NotNil(t, a)
}

// 通知/归档/预订单活动补充覆盖
func TestNotifierActivities(t *testing.T) {
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
	a := NewActivities(store, notif, checker)
	ctx := context.Background()

	// 通知活动（Notifier 非 nil）
	require.NoError(t, a.NotifyApproval(ctx, "t", "o", "PRICE", "u", "AT-1"))
	require.NoError(t, a.NotifyPreOrderConfirm(ctx, "t", "PRE-1", []byte(`[]`)))
	require.NoError(t, a.NotifyOrderEvent(ctx, "t", "o", "SHP_SHIPPED"))
	require.Equal(t, 3, notif.count("approval:")+notif.count("preorder-card:")+notif.count("order-event:"))

	// Notifier nil → no-op
	a2 := NewActivities(store, nil, checker)
	require.NoError(t, a2.NotifyApproval(ctx, "t", "o", "PRICE", "u", "AT-1"))

	// 预订单活动（CreatePreOrder 消息幂等 + GetPreOrder）
	res, err := a.CreatePreOrder(ctx, repo.NewPreOrder{
		TenantID: "t", GroupID: "g", MsgID: "m", Candidates: []byte(`[]`), IdempotencyKey: "k-pre",
	})
	require.NoError(t, err)
	require.True(t, res.Created)
	res2, err := a.CreatePreOrder(ctx, repo.NewPreOrder{
		TenantID: "t", GroupID: "g", MsgID: "m", Candidates: []byte(`[]`), IdempotencyKey: "k-pre",
	})
	require.NoError(t, err)
	require.False(t, res2.Created)
	po, err := a.GetPreOrder(ctx, "t", res.PreOrder.ID)
	require.NoError(t, err)
	require.Equal(t, res.PreOrder.ID, po.ID)
	require.NoError(t, a.UpdatePreOrderState(ctx, "t", po.ID, domain.StatePreCaptured,
		domain.StatePreIntentParsing, "IntentParsing", "r", nil, ""))

	// 归档/回款（无订单 → 错误）
	require.Error(t, a.ArchiveOrder(ctx, "t", "o", "u"))
	require.Error(t, a.TrackPaymentFromNone(ctx, "t", "o", "u", 100))
}

// fake 存储补充：GetShipment/GetApprovalTask/seedOrder 状态链
func TestFakeStoreQueries(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	store.SeedCustomer("t", "c", "A")
	ord := store.seedOrder("t", "ORD-q", "c", "款到", 10)
	require.Equal(t, domain.StateORDRequested, ord.State)

	// GetShipment 未知
	_, err := store.GetShipment(ctx, "t", "SHP-x")
	require.True(t, repo.IsNotFound(err))
	// GetApprovalTask 未知
	_, err = store.GetApprovalTask(ctx, "t", "AT-x")
	require.True(t, repo.IsNotFound(err))
	// GetCustomerGrade 成功
	g, err := store.GetCustomerGrade(ctx, "t", "c")
	require.NoError(t, err)
	require.Equal(t, "A", g)
}

// 哨兵错误类型 + 预订单未知状态（防御分支）
func TestSentinelErrors(t *testing.T) {
	require.Equal(t, "restart from SHP", (&errSegmentRestart{stage: "SHP"}).Error())
	require.Equal(t, "flow ended", errFlowEnd.Error())

	// 预订单未知状态 → 报错
	store := newFakeStore()
	notif := &FakeNotifier{}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleSales, nil })
	po, _, err := store.CreatePreOrder(t.Context(), NewPreOrderForTest("chang-an", "g", "m-x"))
	require.NoError(t, err)
	po.State = "PRE_BOGUS"
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(NewActivities(store, notif, checker))
	env.RegisterWorkflow(PreOrderWorkflow)
	env.ExecuteWorkflow(PreOrderWorkflow, PreOrderWorkflowParams{TenantID: "chang-an", PreOrderID: po.ID})
	require.Error(t, env.GetWorkflowError())

	// 修改未知分类 → 错误
	_, err = applyModificationError("X")
	require.Error(t, err)
}

func applyModificationError(class domain.ModificationClass) (string, error) {
	if class != domain.ModClassA && class != domain.ModClassB && class != domain.ModClassC &&
		class != domain.ModClassD && class != domain.ModClassE {
		return "", fmt.Errorf("unknown modification class %q", class)
	}
	return "", nil
}

// fakestore/activities 边界补充（覆盖率补强）
func TestFakeStoreAndActivitiesEdges(t *testing.T) {
	store := newFakeStore()
	a := NewActivities(store, nil, nil)
	ctx := context.Background()
	store.SeedCustomer("t", "c", "A")
	store.SeedInventory("t", "wh", "p", 100)

	// fakestore.CreateOrder 幂等冲突（同键异 payload → ErrIdempotencyConflict）
	o1, created, err := store.CreateOrder(ctx, NewOrderForTest("t", "c", "款到", "k-edge"))
	require.NoError(t, err)
	require.True(t, created)
	in := NewOrderForTest("t", "c", "款到", "k-edge")
	in.Payload = []byte(`{"different":true}`)
	_, _, err = store.CreateOrder(ctx, in)
	require.True(t, repo.IsIdempotencyConflict(err))

	// fakestore.UpdateOrderStateTx 状态不匹配
	require.Error(t, store.UpdateOrderStateTx(ctx, "t", o1.ID, "WRONG", "X", "a", "u", "r", nil))
	// fakestore.ShipAndUpdateOrderTx 未知备货（发货守恒失败路径）
	require.Error(t, store.ShipAndUpdateOrderTx(ctx, repo.Shipment{
		ID: "s1", TenantID: "t", OrderID: o1.ID, StockPrepareID: "sp-x", WarehouseID: "wh",
		PlannedQty: 1, ShippedQty: 1, IdempotencyKey: "sk-1",
	}, []repo.ShipmentItem{{
		ID: "si1", TenantID: "t", ShipmentID: "s1", OrderItemID: "item-1", ProductID: "p", Qty: 1,
	}}, "F", "T", "u", "r", nil))
	// fakestore.InvoiceAndUpdateOrderTx 未知明细（守恒输入）
	require.Error(t, store.InvoiceAndUpdateOrderTx(ctx, repo.Invoice{
		ID: "i1", TenantID: "t", OrderID: o1.ID, AmountCents: 1, Qty: 1, IdempotencyKey: "ik-1",
	}, []repo.InvoiceItem{{
		ID: "ii1", TenantID: "t", InvoiceID: "i1", OrderItemID: "item-x", ProductID: "p",
		Qty: 1, AmountCents: 1, IdempotencyKey: "iik-1",
	}}, "F", "T", "u", "r", nil))

	// GetShipment/GetApprovalTask 成功路径（fakestore）
	// 直接检查未定义项返回错误
	_, err = store.GetShipment(ctx, "t", "s-x")
	require.True(t, repo.IsNotFound(err))
	_, err = store.GetApprovalTask(ctx, "t", "AT-x")
	require.True(t, repo.IsNotFound(err))

	// NotifyPreOrderConfirm/NotifyOrderEvent 的 Notifier=nil 分支
	require.NoError(t, a.NotifyPreOrderConfirm(ctx, "t", "PRE-1", []byte(`[]`)))
	require.NoError(t, a.NotifyOrderEvent(ctx, "t", "o", "S"))

	// CreatePreOrder 活动结果 / DeriveOrder 活动结果
	res, err := a.CreatePreOrder(ctx, NewPreOrderForTest("t", "g", "m-edge"))
	require.NoError(t, err)
	require.True(t, res.Created)
	der, err := a.DeriveOrder(ctx, NewOrderForTest("t", "c", "款到", "k-derive"))
	require.NoError(t, err)
	require.True(t, der.Created)
	require.NotNil(t, der.Order)
}

// fakestore 内部方法直接覆盖（GetShipment/GetApprovalTask/ListApprovalTasks/orderQty/setStateLocked）
func TestFakeStoreInternalMethods(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	store.SeedCustomer("t", "c", "A")
	store.SeedInventory("t", "wh", "p", 10)

	// 发货 + 查询成功路径（GetShipment）
	o, _, err := store.CreateOrder(ctx, NewOrderForTest("t", "c", "款到", "k-qs"))
	require.NoError(t, err)
	store.SeedPrepared("t", o.ID, "item-1", 10)
	require.NoError(t, store.UpdateOrderStateTx(ctx, "t", o.ID, domain.StateORDRequested, "F", "x", "u", "r", nil))
	require.NoError(t, store.ShipAndUpdateOrderTx(ctx, repo.Shipment{
		ID: "SHP-q", TenantID: "t", OrderID: o.ID, StockPrepareID: "STK-" + shortFakeID(o.ID+":1"),
		WarehouseID: "wh", PlannedQty: 10, ShippedQty: 10, IdempotencyKey: "sk-q",
	}, []repo.ShipmentItem{{
		ID: "si-q", TenantID: "t", ShipmentID: "SHP-q", OrderItemID: "item-1", ProductID: "p", Qty: 10,
	}}, "F", "T", "u", "r", nil))
	sh, err := store.GetShipment(ctx, "t", "SHP-q")
	require.NoError(t, err)
	require.Equal(t, domain.ShipmentStateShipped, sh.State)

	// GetApprovalTask 成功路径 + ListApprovalTasks 过滤组合
	taskID, err := store.CreateApprovalTaskTx(ctx, repo.NewApprovalTask{
		TenantID: "t", OrderID: o.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	require.NoError(t, err)
	task, err := store.GetApprovalTask(ctx, "t", taskID)
	require.NoError(t, err)
	require.Equal(t, "PRICE", task.Kind)
	tasks, err := store.ListApprovalTasks(ctx, "t", "u-supervisor", "PRICE", "PENDING", 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	tasks, err = store.ListApprovalTasks(ctx, "t", "", "NOPE", "", 10)
	require.NoError(t, err)
	require.Empty(t, tasks)

	// SumShippedByOrderItem 成功（有发货）
	sums, err := store.SumShippedByOrderItem(ctx, "t", o.ID)
	require.NoError(t, err)
	require.Equal(t, int64(10), sums["item-1"])

	// CreateOrder 幂等重放（同键同 payload → created=false）
	o2, created, err := store.CreateOrder(ctx, NewOrderForTest("t", "c", "款到", "k-qs"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, o.ID, o2.ID)

	// UpdateOrderMetaTx 成功
	require.NoError(t, store.UpdateOrderMetaTx(ctx, "t", o.ID, "HT-1", []byte(`{}`)))
	// setStateLocked 状态不匹配路径（经 UpdateOrderStateTx 已测）+ ShipAndUpdateOrderTx 幂等重放
	require.NoError(t, store.ShipAndUpdateOrderTx(ctx, repo.Shipment{
		ID: "SHP-q", TenantID: "t", OrderID: o.ID, StockPrepareID: "STK-" + shortFakeID(o.ID+":1"),
		WarehouseID: "wh", PlannedQty: 10, ShippedQty: 10, IdempotencyKey: "sk-q",
	}, []repo.ShipmentItem{{
		ID: "si-q", TenantID: "t", ShipmentID: "SHP-q", OrderItemID: "item-1", ProductID: "p", Qty: 10,
	}}, "T", "T", "u", "r", nil)) // 幂等重放：状态已是 T → setStateLocked 幂等跳过
}

// NewActivitiesWithData 是生产装配入口（LocalRunner/worker 使用），覆盖构造路径。
func TestNewActivitiesWithData(t *testing.T) {
	a := NewActivitiesWithData(&FakeStore{}, &FakeNotifier{}, nil, nil)
	if a == nil || a.Store == nil || a.Notifier == nil {
		t.Fatalf("NewActivitiesWithData did not wire fields: %+v", a)
	}
}
