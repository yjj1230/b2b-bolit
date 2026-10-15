//go:build integration

package repo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
)

// ── Store 契约生产链（txwrappers 全路径：workflows.Store 接口在真实 PG 上的完整链路）──

func TestStoreContractFullChain(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}

	// 1) CreateOrder（幂等）
	key, _ := domain.OrderKey("chang-an", "quick", "store-chain-1")
	payload := []byte(`{"p":"store-chain"}`)
	ord, created, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: payload,
		Items: []NewOrderItem{{
			ID: "item-chain", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 100, UnitPriceCents: 1000,
		}},
	})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	// CreateOrderFromPreOrderTx（同一实现）
	_, created2, err := e.repo.CreateOrderFromPreOrderTx(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: payload,
		Items: []NewOrderItem{{
			ID: "item-chain", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 100, UnitPriceCents: 1000,
		}},
	})
	if err != nil || created2 {
		t.Fatalf("idempotent replay should return existing: %v %v", created2, err)
	}

	// 2) UpdateOrderStateTx（ORD_REQUESTED → PRICE_REVIEWING）
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDRequested,
		domain.StateORDPriceReviewing, "PriceReviewStarted", "u-supervisor", "r", nil); err != nil {
		t.Fatalf("update state: %v", err)
	}

	// 3) CreateApprovalTaskTx + DecideTaskTx
	taskID, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := e.repo.DecideTaskTx(ctx, "chang-an", taskID, "APPROVED", "通过", "u-supervisor", nil); err != nil {
		t.Fatalf("decide task: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDPriceReviewing,
		domain.StateORDPriceApproved, "PriceReviewed", "u-supervisor", "r", nil); err != nil {
		t.Fatalf("price approved: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDPriceApproved,
		domain.StateORDContractGenerate, "ContractGenerateStarted", "system", "r", nil); err != nil {
		t.Fatalf("contract gen: %v", err)
	}
	if err := e.repo.SetOrderContractTx(ctx, "chang-an", ord.ID, "HT-CHAIN-1"); err != nil {
		t.Fatalf("set contract: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDContractGenerate,
		domain.StateORDContracted, "ContractGenerated", "system", "r", nil); err != nil {
		t.Fatalf("contracted: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDContracted,
		domain.StateSTKReadyForStock, "ReadyForStock", "system", "r", nil); err != nil {
		t.Fatalf("ready for stock: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKReadyForStock,
		domain.StateSTKStockReviewing, "StockReviewStarted", "system", "r", nil); err != nil {
		t.Fatalf("stock reviewing: %v", err)
	}

	// 4) ReserveStockTx（备货 + 状态迁移同事务）
	sp := StockPrepare{
		ID: "STK-chain", TenantID: "chang-an", OrderID: ord.ID, WarehouseID: "wh-yangyuan",
		State: domain.StateSTKWarehouseConfirm, PlannedQty: 100, RemainingQty: 100,
		BatchSeq: 1, IdempotencyKey: "sp-chain-1",
	}
	spItems := []StockPrepareItem{{
		ID: "spi-chain", TenantID: "chang-an", StockPrepareID: "STK-chain",
		OrderItemID: "item-chain", ProductID: "prod-c68", Qty: 100,
	}}
	if err := e.repo.ReserveStockTx(ctx, sp, spItems, domain.StateSTKStockReviewing,
		domain.StateSTKWarehouseConfirm, "u-op", "商务备货", nil); err != nil {
		t.Fatalf("reserve stock: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKWarehouseConfirm,
		domain.StateSTKWarehouseConfirmed, "WarehouseConfirmed", "u-wh-yangyuan", "r", nil); err != nil {
		t.Fatalf("warehouse confirmed: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKWarehouseConfirmed,
		domain.StateSTKPaymentConfirming, "PaymentConfirmStarted", "system", "r", nil); err != nil {
		t.Fatalf("payment confirming: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKPaymentConfirming,
		domain.StateSTKPaymentConfirmed, "PaymentConfirmed", "u-accounting", "r", nil); err != nil {
		t.Fatalf("payment confirmed: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKPaymentConfirmed,
		domain.StateSTKShipOrderIssued, "ShipOrderIssued", "u-op", "r", nil); err != nil {
		t.Fatalf("ship order issued: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKShipOrderIssued,
		domain.StateSHPShipAssigning, "ShipAssignStarted", "system", "r", nil); err != nil {
		t.Fatalf("ship assigning: %v", err)
	}

	// 5) ShipAndUpdateOrderTx（发货 + 库存扣减 + 状态迁移）
	ship := Shipment{
		ID: "SHP-chain", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: "STK-chain",
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-chain-1",
	}
	shipItems := []ShipmentItem{{
		ID: "si-chain", TenantID: "chang-an", ShipmentID: "SHP-chain",
		OrderItemID: "item-chain", ProductID: "prod-c68", Qty: 100,
	}}
	if err := e.repo.ShipAndUpdateOrderTx(ctx, ship, shipItems, domain.StateSHPShipAssigning,
		domain.StateSHPShipPed, "u-wh-yangyuan", "实际发货", nil); err != nil {
		t.Fatalf("ship goods: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSHPShipPed,
		domain.StateSHPFullyShipped, "FullyShipped", "system", "r", nil); err != nil {
		t.Fatalf("fully shipped: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSHPFullyShipped,
		domain.StateINVReadyForInvoice, "ReadyForInvoice", "system", "r", nil); err != nil {
		t.Fatalf("ready invoice: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateINVReadyForInvoice,
		domain.StateINVInvoiceApplying, "InvoiceApplyStarted", "system", "r", nil); err != nil {
		t.Fatalf("invoice applying: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateINVInvoiceApplying,
		domain.StateINVInvoicing, "InvoiceSubmitStarted", "system", "r", nil); err != nil {
		t.Fatalf("invoicing: %v", err)
	}

	// 6) InvoiceAndUpdateOrderTx（守恒校验 + 状态迁移）
	inv := Invoice{
		ID: "INV-chain", TenantID: "chang-an", OrderID: ord.ID, State: domain.InvoiceStateIssued,
		AmountCents: 100000, Qty: 100, IdempotencyKey: "inv-chain-1",
	}
	invItems := []InvoiceItem{{
		ID: "ii-chain", TenantID: "chang-an", InvoiceID: "INV-chain",
		OrderItemID: "item-chain", ProductID: "prod-c68", Qty: 100, AmountCents: 100000,
		IdempotencyKey: "invitem-chain-1",
	}}
	if err := e.repo.InvoiceAndUpdateOrderTx(ctx, inv, invItems, domain.StateINVInvoicing,
		domain.StateINVInvoiced, "u-accounting", "开票完成", nil); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateINVInvoiced,
		domain.StateINVPaymentTracking, "PaymentTrackingStarted", "system", "r", nil); err != nil {
		t.Fatalf("payment tracking: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateINVPaymentTracking,
		domain.StateINVContractArchived, "ContractArchived", "u-accounting", "r", nil); err != nil {
		t.Fatalf("archived: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateINVContractArchived,
		domain.StateINVClosed, "OrderClosed", "u-accounting", "r", nil); err != nil {
		t.Fatalf("closed: %v", err)
	}

	// 终态校验 + 守恒聚合
	o, err := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if err != nil || o.State != domain.StateINVClosed {
		t.Fatalf("final state: %s %v", o.State, err)
	}
	rows, err := e.repo.ReconcileAllOrders(ctx, "chang-an")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	found := false
	for _, rc := range rows {
		if rc.OrderID == ord.ID {
			found = true
			if len(rc.Violations) != 0 {
				t.Fatalf("violations: %v", rc.Violations)
			}
		}
	}
	if !found {
		t.Fatal("order missing from reconcile")
	}
	// 对账脚本同源：负库存清单为空
	neg, err := e.repo.ReconcileInventory(ctx, "chang-an")
	if err != nil || len(neg) != 0 {
		t.Fatalf("negative inventory: %v %v", neg, err)
	}

	// 7) 预订单链（UpdatePreOrderStateTx / CreatePreOrder / GetPreOrderByMsg / ListPreOrders）
	preKey, _ := domain.PreOrderKey("chang-an", "g-chain", "m-chain", "v1")
	po, created3, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g-chain", MsgID: "m-chain",
		Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil || !created3 {
		t.Fatalf("create preorder: %v %v", created3, err)
	}
	if err := e.repo.UpdatePreOrderStateTx(ctx, "chang-an", po.ID, domain.StatePreCaptured,
		domain.StatePreIntentParsing, "IntentParsing", "r", nil, ""); err != nil {
		t.Fatalf("preorder intent: %v", err)
	}
	if err := e.repo.UpdatePreOrderStateTx(ctx, "chang-an", po.ID, domain.StatePreIntentParsing,
		domain.StatePrePendingConfirm, "IntentParsed", "r", nil, ""); err != nil {
		t.Fatalf("preorder pending: %v", err)
	}
	po2, err := e.repo.GetPreOrderByMsg(ctx, "chang-an", "g-chain", "m-chain")
	if err != nil || po2.ID != po.ID {
		t.Fatalf("get by msg: %v %v", po2, err)
	}
	pres, err := e.repo.ListPreOrders(ctx, "chang-an", 10)
	if err != nil || len(pres) != 1 {
		t.Fatalf("list preorders: %v %v", pres, err)
	}
	// LookupBinding + SeedWecomBinding
	if err := e.repo.SeedWecomBinding(ctx, "chang-an", "ext-1", "g-chain", []byte(`{"owner":"u-sales"}`)); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	tenant, binding, err := e.repo.LookupBinding(ctx, "g-chain")
	if err != nil || tenant != "chang-an" || len(binding) == 0 {
		t.Fatalf("lookup binding: %s %v %v", tenant, binding, err)
	}
	if _, _, err := e.repo.LookupBinding(ctx, "g-nope"); !IsNotFound(err) {
		t.Fatalf("lookup binding unknown: %v", err)
	}

	// 基础设施边缘：Pool 非空 / now() 可用
	if e.repo.Pool() == nil {
		t.Fatal("pool must be non-nil")
	}
	if now().IsZero() {
		t.Fatal("now must return a time")
	}
	// ListOrders 按租户+状态过滤
	all, err := e.repo.ListOrders(ctx, "", "", 100)
	if err != nil || len(all) == 0 {
		t.Fatalf("list all: %v %v", len(all), err)
	}
	closed, err := e.repo.ListOrders(ctx, "chang-an", domain.StateINVClosed, 100)
	if err != nil || len(closed) != 1 {
		t.Fatalf("list closed: %d %v", len(closed), err)
	}
	// ReconcileInventory 租户过滤
	neg2, err := e.repo.ReconcileInventory(ctx, "zhichu")
	if err != nil || len(neg2) != 0 {
		t.Fatalf("reconcile inventory tenant filter: %v %v", neg2, err)
	}
	// SumShippedByOrderItemTx（事务内变体）
	tx2, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx2.Rollback(ctx)
	sums2, err := e.repo.SumShippedByOrderItemTx(ctx, tx2, "chang-an", ord.ID)
	if err != nil {
		t.Fatalf("sum tx: %v", err)
	}
	if sums2["item-chain"] != 100 {
		t.Fatalf("sum tx value: %v", sums2)
	}
}

// 具体错误分支：引用缺失（rowlock ErrNoRows / 守恒输入非法）
func TestRepoReferenceErrorBranches(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)

	// ApplyShipment：未知备货单 → ErrNotFound（行锁 ErrNoRows 分支）
	tx, _ := e.repo.Begin(ctx)
	err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-x", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: "STK-nope",
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10,
		ShippedQty: 10, BatchSeq: 1, IdempotencyKey: "ship-x-1",
	}, []ShipmentItem{{
		ID: "si-x", TenantID: "chang-an", ShipmentID: "shp-x",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan")
	if !IsNotFound(err) {
		tx.Rollback(ctx)
		t.Fatalf("unknown stock prepare should 404: %v", err)
	}
	tx.Rollback(ctx)

	// ApplyShipment：备货单无此明细 → ErrNotFound
	tx2, _ := e.repo.Begin(ctx)
	err = e.repo.ApplyShipment(ctx, tx2, Shipment{
		ID: "shp-y", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10,
		ShippedQty: 10, BatchSeq: 1, IdempotencyKey: "ship-y-1",
	}, []ShipmentItem{{
		ID: "si-y", TenantID: "chang-an", ShipmentID: "shp-y",
		OrderItemID: "item-unknown", ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan")
	if !IsNotFound(err) {
		tx2.Rollback(ctx)
		t.Fatalf("unknown order_item in prepare should 404: %v", err)
	}
	tx2.Rollback(ctx)

	// ApplyInvoice：未知订单 → ErrNotFound（行锁）
	tx3, _ := e.repo.Begin(ctx)
	err = e.repo.ApplyInvoice(ctx, tx3, Invoice{
		ID: "inv-x", TenantID: "chang-an", OrderID: "ORD-nope", State: domain.InvoiceStateIssued,
		AmountCents: 100, Qty: 1, IdempotencyKey: "inv-x-1",
	}, []InvoiceItem{{
		ID: "ii-x", TenantID: "chang-an", InvoiceID: "inv-x", OrderItemID: item.ID,
		ProductID: "prod-c68", Qty: 1, AmountCents: 100, IdempotencyKey: "invitem-x-1",
	}}, "u-accounting")
	if !IsNotFound(err) {
		tx3.Rollback(ctx)
		t.Fatalf("unknown order should 404: %v", err)
	}
	tx3.Rollback(ctx)

	// ApplyInvoice：未知明细 → ErrNotFound
	tx4, _ := e.repo.Begin(ctx)
	err = e.repo.ApplyInvoice(ctx, tx4, Invoice{
		ID: "inv-y", TenantID: "chang-an", OrderID: ord.ID, State: domain.InvoiceStateIssued,
		AmountCents: 100, Qty: 1, IdempotencyKey: "inv-y-1",
	}, []InvoiceItem{{
		ID: "ii-y", TenantID: "chang-an", InvoiceID: "inv-y", OrderItemID: "item-unknown",
		ProductID: "prod-c68", Qty: 1, AmountCents: 100, IdempotencyKey: "invitem-y-1",
	}}, "u-accounting")
	if !IsNotFound(err) {
		tx4.Rollback(ctx)
		t.Fatalf("unknown order_item should 404: %v", err)
	}
	tx4.Rollback(ctx)

	// 查询成功路径：GetShipment / GetInvoice / GetApprovalTask / GetCustomerGrade / SumShipped
	tx5, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx5, Shipment{
		ID: "shp-ok2", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-ok2-1",
	}, []ShipmentItem{{
		ID: "si-ok2", TenantID: "chang-an", ShipmentID: "shp-ok2",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if err := tx5.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sh, err := e.repo.GetShipment(ctx, "chang-an", "shp-ok2")
	if err != nil || sh.State != domain.ShipmentStateShipped {
		t.Fatalf("get shipment: %v %v", sh, err)
	}
	sums, err := e.repo.SumShippedByOrderItem(ctx, "chang-an", ord.ID)
	if err != nil || sums[item.ID] != 100 {
		t.Fatalf("sum shipped non-tx: %v %v", sums, err)
	}
	// D-006 后业务流程不再经本地 customers；此处只验证 legacy 方法仍可回读旧数据。
	if err := e.repo.SeedCustomer(ctx, "chang-an", "cust-0001", "北京华信科技", "A", "u-sales"); err != nil {
		t.Fatalf("seed legacy customer: %v", err)
	}
	grade, err := e.repo.GetCustomerGrade(ctx, "chang-an", "cust-0001")
	if err != nil || grade != "A" {
		t.Fatalf("grade: %s %v", grade, err)
	}
	taskID, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task, err := e.repo.GetApprovalTask(ctx, "chang-an", taskID)
	if err != nil || task.Kind != "PRICE" {
		t.Fatalf("get task: %v %v", task, err)
	}
	// 同 kind 已存在 PENDING → 返回既有；全 PENDING 已占 → ErrAlreadyExists 分支
	taskID2, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err != nil || taskID2 != taskID {
		t.Fatalf("dup task should return existing: %v %v", taskID2, err)
	}
	if err := e.repo.DecideTaskTx(ctx, "chang-an", taskID, "APPROVED", "ok", "u-supervisor", nil); err != nil {
		t.Fatalf("decide: %v", err)
	}
	// 已裁决后同 kind 再建 → 新任务（无 PENDING）→ 成功（非 ErrAlreadyExists）
	taskID3, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err != nil || taskID3 == taskID {
		t.Fatalf("new task after decision: %v %v", taskID3, err)
	}
	// SeedDefaultWorld 重复播种幂等
	if err := e.repo.SeedDefaultWorld(ctx, "run-2"); err != nil {
		t.Fatalf("reseed: %v", err)
	}
}

// txwrappers commit 失败注入（触发器：最后一条语句抛错 → Commit 前回滚）
func TestTxWrapperCommitInjection(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)

	// UpdateOrderMetaTx：orders UPDATE 后触发器抛错 → 元数据不生效
	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_meta() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: meta update failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_meta BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION fail_meta();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_meta ON orders; DROP FUNCTION IF EXISTS fail_meta();`)
	if err := e.repo.UpdateOrderMetaTx(ctx, "chang-an", ord.ID, "HT-X", []byte(`{"x":1}`)); err == nil {
		t.Fatal("injected update failure should error")
	}
	o, _ := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.ContractNo != nil {
		t.Fatal("meta update should be rolled back")
	}
	e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_meta ON orders; DROP FUNCTION IF EXISTS fail_meta();`)

	// SetOrderContractTx / UpdateResubmitChainTx / RecordFlowFailed / UpdatePreOrderStateTx 成功路径
	if err := e.repo.SetOrderContractTx(ctx, "chang-an", ord.ID, "HT-1"); err != nil {
		t.Fatalf("set contract: %v", err)
	}
	if err := e.repo.UpdateResubmitChainTx(ctx, "chang-an", ord.ID, 2, ord.ID); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if err := e.repo.RecordFlowFailed(ctx, "chang-an", ord.ID, "inject", nil); err != nil {
		t.Fatalf("flow failed: %v", err)
	}
	preKey, _ := domain.PreOrderKey("chang-an", "g1", "m1", "v1")
	po, _, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil {
		t.Fatalf("preorder: %v", err)
	}
	if err := e.repo.UpdatePreOrderStateTx(ctx, "chang-an", po.ID, domain.StatePreCaptured,
		domain.StatePreIntentParsing, "IntentParsing", "r", nil, ""); err != nil {
		t.Fatalf("preorder state: %v", err)
	}
	// CloseShipment 成功路径
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 10)
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-close", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10,
		ShippedQty: 10, BatchSeq: 1, IdempotencyKey: "ship-close-1",
	}, []ShipmentItem{{
		ID: "si-close", TenantID: "chang-an", ShipmentID: "shp-close",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	txC, _ := e.repo.Begin(ctx)
	if err := e.repo.CloseShipment(ctx, txC, "chang-an", "shp-close", "u-op"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := txC.Commit(ctx); err != nil {
		t.Fatalf("commit close: %v", err)
	}
	sh, _ := e.repo.GetShipment(ctx, "chang-an", "shp-close")
	if sh.State != domain.ShipmentStateClosed {
		t.Fatalf("shipment state: %s", sh.State)
	}
}

// GetInvoice/GetCustomerContact/GetArchiveCursor/AdvanceArchiveCursor 成功与边界路径
func TestRepoQuerySuccessPaths(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)

	// 发货 → 开票 → GetInvoice 成功
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-q1", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-q1",
	}, []ShipmentItem{{
		ID: "si-q1", TenantID: "chang-an", ShipmentID: "shp-q1",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tx2, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyInvoice(ctx, tx2, Invoice{
		ID: "inv-q1", TenantID: "chang-an", OrderID: ord.ID, State: domain.InvoiceStateIssued,
		AmountCents: 100000, Qty: 100, IdempotencyKey: "inv-q1",
	}, []InvoiceItem{{
		ID: "ii-q1", TenantID: "chang-an", InvoiceID: "inv-q1",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100, AmountCents: 100000,
		IdempotencyKey: "invitem-q1",
	}}, "u-accounting"); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	inv, err := e.repo.GetInvoice(ctx, "chang-an", "inv-q1")
	require.NoError(t, err)
	require.Equal(t, int64(100000), inv.AmountCents)

	// GetCustomerContact 成功 + 旋转
	_, err = e.repo.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "li")
	require.NoError(t, err)
	contact, err := e.repo.GetCustomerContact(ctx, "chang-an", "cust-0001")
	require.NoError(t, err)
	require.Equal(t, "li", contact)

	// AdvanceArchiveCursor 边界：同值推进（>= 语义）+ 跨租户独立
	require.NoError(t, e.repo.AdvanceArchiveCursor(ctx, "chang-an", 100))
	require.NoError(t, e.repo.AdvanceArchiveCursor(ctx, "chang-an", 100)) // 同值允许
	seq, _ := e.repo.GetArchiveCursor(ctx, "chang-an")
	require.Equal(t, int64(100), seq)
	// 跨租户独立游标（先建租户满足 FK）
	require.NoError(t, e.repo.SeedTenant(ctx, "zhichu", "智厨"))
	require.NoError(t, e.repo.AdvanceArchiveCursor(ctx, "zhichu", 5))
	seq2, _ := e.repo.GetArchiveCursor(ctx, "zhichu")
	require.Equal(t, int64(5), seq2)
	seq3, _ := e.repo.GetArchiveCursor(ctx, "chang-an")
	require.Equal(t, int64(100), seq3)
}
