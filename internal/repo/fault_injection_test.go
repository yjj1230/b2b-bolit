//go:build integration

package repo

import (
	"context"
	"testing"

	"b2b_pilot_v3/internal/domain"
)

// ── 错误注入（docs/03-testing.html §5：触发器故障钩子 → 事务中途失败 → 全量回滚）──

// 发货中途失败（INSERT shipments 触发器抛错）→ 库存未扣、事件未写、发货单不残留
func TestFaultInjectionShipmentMidTx(t *testing.T) {
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

	// 注入触发器：INSERT shipments 即抛错
	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_shipment() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: shipment insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_ship BEFORE INSERT ON shipments FOR EACH ROW EXECUTE FUNCTION fail_shipment();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_ship ON shipments; DROP FUNCTION IF EXISTS fail_shipment();`)

	// 事务中途失败 → 全量回滚
	err := e.repo.ShipAndUpdateOrderTx(ctx, Shipment{
		ID: "SHP-fail", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-fail-1",
	}, []ShipmentItem{{
		ID: "si-fail", TenantID: "chang-an", ShipmentID: "SHP-fail",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, domain.StateSHPShipAssigning, domain.StateSHPShipPed, "u-wh-yangyuan", "r", nil)
	if err == nil {
		t.Fatal("injected failure should error")
	}

	// 无残留：库存不变、发货单不存在、订单状态不变、事件未追加
	inv, _ := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if inv.Qty != 100 {
		t.Fatalf("inventory after failed tx = %d, want 100", inv.Qty)
	}
	if _, err := e.repo.GetShipment(ctx, "chang-an", "SHP-fail"); !IsNotFound(err) {
		t.Fatalf("failed shipment should not exist: %v", err)
	}
	o, _ := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.State != domain.StateORDRequested {
		t.Fatalf("order state changed despite failure: %s", o.State)
	}
	events, _ := e.repo.ListOrderEvents(ctx, "chang-an", ord.ID, 100)
	for _, ev := range events {
		if ev.Action == "GoodsShipped" {
			t.Fatal("shipment event written despite rollback")
		}
	}
}

// 开票中途失败（INSERT invoice_items 触发器抛错）→ 发票/明细/事件全量回滚
func TestFaultInjectionInvoiceMidTx(t *testing.T) {
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
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-fi", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-fi-1",
	}, []ShipmentItem{{
		ID: "si-fi", TenantID: "chang-an", ShipmentID: "shp-fi",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("shipment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_invoice() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: invoice insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_inv BEFORE INSERT ON invoice_items FOR EACH ROW EXECUTE FUNCTION fail_invoice();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_inv ON invoice_items; DROP FUNCTION IF EXISTS fail_invoice();`)

	err := e.repo.InvoiceAndUpdateOrderTx(ctx, Invoice{
		ID: "INV-fi", TenantID: "chang-an", OrderID: ord.ID, State: domain.InvoiceStateIssued,
		AmountCents: 100000, Qty: 100, IdempotencyKey: "inv-fi-1",
	}, []InvoiceItem{{
		ID: "ii-fi", TenantID: "chang-an", InvoiceID: "INV-fi",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100, AmountCents: 100000,
		IdempotencyKey: "invitem-fi-1",
	}}, domain.StateINVInvoicing, domain.StateINVInvoiced, "u-accounting", "r", nil)
	if err == nil {
		t.Fatal("injected failure should error")
	}
	if _, err := e.repo.GetInvoice(ctx, "chang-an", "INV-fi"); !IsNotFound(err) {
		t.Fatalf("failed invoice should not exist: %v", err)
	}
	o, _ := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.State != domain.StateORDRequested {
		t.Fatalf("order state changed despite invoice failure: %s", o.State)
	}
}

// 建单中途失败（order_items 触发器抛错）→ 订单/明细/事件全量回滚
func TestFaultInjectionCreateOrderMidTx(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_order_item() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: order_item insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_oi BEFORE INSERT ON order_items FOR EACH ROW EXECUTE FUNCTION fail_order_item();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_oi ON order_items; DROP FUNCTION IF EXISTS fail_order_item();`)

	key, _ := domain.OrderKey("chang-an", "quick", "fail-1")
	_, _, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{}`),
		Items: []NewOrderItem{{
			ID: "item-f1", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 10, UnitPriceCents: 100,
		}},
	})
	if err == nil {
		t.Fatal("injected failure should error")
	}
	// 订单不残留
	var n int
	_ = e.conn.QueryRow(ctx, `SELECT count(*) FROM orders WHERE idempotency_key=$1`, key).Scan(&n)
	if n != 0 {
		t.Fatal("order row should be rolled back")
	}
	// 同键重试（故障解除后）→ 成功
	e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_oi ON order_items; DROP FUNCTION IF EXISTS fail_order_item();`)
	ord, created, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{}`),
		Items: []NewOrderItem{{
			ID: "item-f1", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 10, UnitPriceCents: 100,
		}},
	})
	if err != nil || !created {
		t.Fatalf("retry after fault: %v %v", created, err)
	}
	_ = ord
}

// 事件追加中途失败（order_events 触发器抛错）→ 状态迁移不生效（同事务）
func TestFaultInjectionEventAppend(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_event() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: event append failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_ev BEFORE INSERT ON order_events FOR EACH ROW EXECUTE FUNCTION fail_event();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_ev ON order_events; DROP FUNCTION IF EXISTS fail_event();`)

	err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateORDRequested,
		domain.StateORDPriceReviewing, "PriceReviewStarted", "u", "r", nil)
	if err == nil {
		t.Fatal("injected event failure should error")
	}
	// 状态未变（同事务回滚）
	o, _ := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.State != domain.StateORDRequested {
		t.Fatalf("state changed despite event failure: %s", o.State)
	}
}

// 建单事件追加失败（order_events 触发器 → CreateOrder 的 appendOrderEvent 错误分支）
func TestFaultInjectionCreateOrderEventAppend(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_ev2() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: order_events append failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_ev2 BEFORE INSERT ON order_events FOR EACH ROW EXECUTE FUNCTION fail_ev2();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_ev2 ON order_events; DROP FUNCTION IF EXISTS fail_ev2();`)

	key, _ := domain.OrderKey("chang-an", "quick", "fail-ev2")
	_, _, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{}`),
		Items: []NewOrderItem{{
			ID: "item-f2", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 10, UnitPriceCents: 100,
		}},
	})
	if err == nil {
		t.Fatal("injected event failure should error")
	}
	var n int
	_ = e.conn.QueryRow(ctx, `SELECT count(*) FROM orders WHERE idempotency_key=$1`, key).Scan(&n)
	if n != 0 {
		t.Fatal("order should be rolled back on event append failure")
	}
}

// 审批任务创建失败（approval_tasks 触发器 → CreateApprovalTask insert 错误分支）
func TestFaultInjectionApprovalTaskInsert(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_at() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: approval_tasks insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_at BEFORE INSERT ON approval_tasks FOR EACH ROW EXECUTE FUNCTION fail_at();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_at ON approval_tasks; DROP FUNCTION IF EXISTS fail_at();`)

	_, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err == nil {
		t.Fatal("injected approval task failure should error")
	}
}

// 种子数据失败分支（users 触发器 → SeedDefaultWorld 错误返回）
func TestFaultInjectionSeedWorld(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_user() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: users insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_user BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION fail_user();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_user ON users; DROP FUNCTION IF EXISTS fail_user();`)

	if err := e.repo.SeedDefaultWorld(ctx, "fail-run"); err == nil {
		t.Fatal("injected seed failure should error")
	}
	// SeedTenant 失败分支（tenants 触发器）
	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_tenant() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: tenants insert failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_tenant BEFORE INSERT ON tenants FOR EACH ROW EXECUTE FUNCTION fail_tenant();`); err != nil {
		t.Fatalf("create trigger2: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_tenant ON tenants; DROP FUNCTION IF EXISTS fail_tenant();`)
	if err := e.repo.SeedTenant(ctx, "fail-tenant", "x"); err == nil {
		t.Fatal("injected tenant failure should error")
	}
}
