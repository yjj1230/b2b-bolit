//go:build integration

package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// L1-01 · CHECK 约束：负库存/负数量/超量发货被拒（防线 1；CQ-06/07/08）
func TestL101CheckConstraints(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 负库存直接 INSERT 被拒
	_, err := e.conn.Exec(ctx, `INSERT INTO inventory_items (tenant_id, warehouse_id, product_id, qty, version)
		VALUES ('chang-an','wh-yangyuan','prod-d90',-1,1)`)
	if err == nil {
		t.Fatal("negative inventory INSERT should be rejected by CHECK")
	}

	// 负/零订单数量被拒
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 10); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	_, err = e.conn.Exec(ctx, `INSERT INTO order_items (id, tenant_id, order_id, product_id, quantity, unit_price_cents)
		VALUES ('bad-neg',$1,$2,'prod-c68',0,100)`, "chang-an", ord.ID)
	if err == nil {
		t.Fatal("zero quantity order_item should be rejected")
	}
	_ = item

	// 超量发货：shipped_qty > planned_qty 被拒
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)
	_, err = e.conn.Exec(ctx, `INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id,
		state, planned_qty, shipped_qty, batch_seq, idempotency_key)
		VALUES ('shp-over',$1,$2,$3,'wh-yangyuan','SHIPPED',100,101,1,'over-ship-key')`,
		"chang-an", ord.ID, spID)
	if err == nil {
		t.Fatal("over-shipment (shipped_qty > planned_qty) should be rejected by CHECK")
	}
}

// L1-02 · 幂等唯一：订单/预订单/发货/开票键冲突路径（含跨租户同键各自生效；防线 3；CQ-11/14）
func TestL102IdempotencyUnique(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	key, _ := domain.OrderKey("chang-an", "quick", "same-uuid")
	payload := []byte(`{"a":1}`)
	ord1, created1, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: payload,
		Items: []NewOrderItem{{ID: "item-a", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"), Quantity: 10, UnitPriceCents: 100}},
	})
	if err != nil || !created1 {
		t.Fatalf("first create: created=%v err=%v", created1, err)
	}
	// 同键同 payload → 返回既有
	ord2, created2, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: payload,
		Items: []NewOrderItem{{ID: "item-a", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"), Quantity: 10, UnitPriceCents: 100}},
	})
	if err != nil || created2 || ord2.ID != ord1.ID {
		t.Fatalf("same key same payload: created=%v id=%s err=%v", created2, ord2.ID, err)
	}
	// 同键异 payload → 409
	_, _, err = e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{"a":2}`),
		Items: []NewOrderItem{{ID: "item-a", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"), Quantity: 10, UnitPriceCents: 100}},
	})
	if err == nil || !IsIdempotencyConflict(err) {
		t.Fatalf("same key different payload should 409, got %v", err)
	}

	// 跨租户同 idempotency_key 各自生效、互不返回对方资源（review 202608061321 P1）
	if err := e.repo.SeedTenant(ctx, "zhichu", "智厨"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := e.repo.SeedUser(ctx, "zhichu", "z-user", "智厨用户", "SALES", ""); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := e.repo.SeedCustomer(ctx, "zhichu", "z-cust", "智厨客户", "B", "z-user"); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if err := e.repo.SeedProduct(ctx, "zhichu", "z-prod", "SKU-Z1", "智厨产品"); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := e.repo.SeedWarehouse(ctx, "zhichu", "z-wh", "智厨仓"); err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}
	ordZ, createdZ, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "zhichu", CustomerID: "z-cust", PaymentTerm: "非款到", SupplierChannel: "渠道B",
		IdempotencyKey: key, Payload: payload,
		Items: []NewOrderItem{{ID: "z-item", ProductID: "z-prod", WarehouseID: strPtr("z-wh"), Quantity: 5, UnitPriceCents: 99}},
	})
	if err != nil || !createdZ {
		t.Fatalf("cross-tenant same key: created=%v err=%v", createdZ, err)
	}
	if ordZ.ID == ord1.ID {
		t.Fatal("cross-tenant same key must not collide")
	}
	// 租户 A 查不到租户 B 的订单
	if _, err := e.repo.GetOrder(ctx, "chang-an", ordZ.ID); !IsNotFound(err) {
		t.Fatalf("tenant A should not see tenant B order, got %v", err)
	}

	// 预订单消息幂等：同一 (tenant, group, msgid) 只建一个
	preKey, _ := domain.PreOrderKey("chang-an", "g1", "m1", "v1")
	po1, createdP1, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil || !createdP1 {
		t.Fatalf("preorder first: %v %v", createdP1, err)
	}
	po2, createdP2, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil || createdP2 || po2.ID != po1.ID {
		t.Fatalf("preorder dup msg: %v %v", createdP2, err)
	}

	// 发货/开票幂等键唯一约束（直接 SQL 验证 UNIQUE 生效）
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	item := e.getOrderItem(ord1.ID)
	spID := e.createTestPrepare(ord1.ID, item.ID, "wh-yangyuan", 10)
	_, err = e.conn.Exec(ctx, `INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id,
		state, planned_qty, shipped_qty, batch_seq, idempotency_key)
		VALUES ('shp-1',$1,$2,$3,'wh-yangyuan','OPEN',10,0,1,'dup-ship-key')`, "chang-an", ord1.ID, spID)
	if err != nil {
		t.Fatalf("insert shipment 1: %v", err)
	}
	_, err = e.conn.Exec(ctx, `INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id,
		state, planned_qty, shipped_qty, batch_seq, idempotency_key)
		VALUES ('shp-1b',$1,$2,$3,'wh-yangyuan','OPEN',10,0,1,'dup-ship-key')`, "chang-an", ord1.ID, spID)
	if err == nil {
		t.Fatal("duplicate shipment idempotency key should be rejected")
	}
}

// L1-03 · 行锁并发：并发发货不超备货（防线 2；CQ-07/14）。
// 0.1 M1 锁库存暂缓：库存不参与校验（发货守恒 = ∑发货 ≤ ∑备货；备货单行锁串行化）。
func TestL103RowLockConcurrency(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 200)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 200)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx, err := e.repo.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			err = e.repo.ApplyShipment(ctx, tx, Shipment{
				ID: fmt.Sprintf("shp-%d", n), TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
				WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 200,
				ShippedQty: 30, BatchSeq: 1, IdempotencyKey: fmt.Sprintf("ship:%d", n),
			}, []ShipmentItem{{
				ID: fmt.Sprintf("si-%d", n), TenantID: "chang-an", ShipmentID: fmt.Sprintf("shp-%d", n),
				OrderItemID: item.ID, ProductID: "prod-c68", Qty: 30,
			}}, "u-wh-yangyuan")
			if err == nil {
				errs <- tx.Commit(ctx)
			} else {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	success, failures := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			failures++
		}
	}
	// 备货 200 / 每次 30 → 最多 6 次成功（第 7 次起 ∑发货>备货 守恒拒绝）
	if success != 6 {
		t.Fatalf("expected 6 successful shipments (200/30), got %d (failures=%d)", success, failures)
	}
	// 0.1 M1：锁库存暂缓 —— 库存不扣减（保持 100）
	inv, err := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if err != nil {
		t.Fatalf("get inventory: %v", err)
	}
	if inv.Qty != 100 {
		t.Fatalf("0.1 inventory must be untouched (M1), got %d", inv.Qty)
	}
}

// L1-04 · 单事务：发货单 + 明细原子提交/回滚（防线 4；CQ-07）。
// 0.1 M1：库存不扣减；守恒口径 = ∑发货 ≤ ∑备货。
func TestL104ShipmentAtomicity(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 50); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)

	// 成功路径：发货 40（0.1 不扣库存）
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-ok", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100, ShippedQty: 40,
		BatchSeq: 1, IdempotencyKey: "ship-ok",
	}, []ShipmentItem{{
		ID: "si-ok", TenantID: "chang-an", ShipmentID: "shp-ok",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 40,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("apply shipment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	inv, _ := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if inv.Qty != 50 {
		t.Fatalf("0.1 inventory untouched (M1), got %d want 50", inv.Qty)
	}

	// 失败路径：超备货发货（40+100 > 备货 100）→ 守恒拒绝（发货单不残留）
	tx2, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	defer tx2.Rollback(ctx)
	err = e.repo.ApplyShipment(ctx, tx2, Shipment{
		ID: "shp-fail", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100, ShippedQty: 100,
		BatchSeq: 2, IdempotencyKey: "ship-fail",
	}, []ShipmentItem{{
		ID: "si-fail", TenantID: "chang-an", ShipmentID: "shp-fail",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, "u-wh-yangyuan")
	if err == nil {
		tx2.Rollback(ctx)
		t.Fatal("over-prepared shipment should fail (ship conservation)")
	}
	_ = tx2.Rollback(ctx)
	// 发货单未残留
	if _, err := e.repo.GetShipment(ctx, "chang-an", "shp-fail"); !IsNotFound(err) {
		t.Fatalf("failed shipment should not exist, got %v", err)
	}
	// 库存不变（M1）
	inv2, _ := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if inv2.Qty != 50 {
		t.Fatalf("inventory after rollback = %d, want 50", inv2.Qty)
	}
	// 库存守恒 CHECK 兜底：直接扣到负数被拒
	_, err = e.conn.Exec(ctx, `UPDATE inventory_items SET qty = -5 WHERE tenant_id='chang-an' AND warehouse_id='wh-yangyuan' AND product_id='prod-c68'`)
	if err == nil {
		t.Fatal("CHECK(qty>=0) should reject negative inventory UPDATE")
	}
}

// L1-05 · 单事务：开票数量+金额双守恒（任一维度超限均拒绝；防线 4/5；CQ-09）
func TestL105InvoiceConservation(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100) // 单价 1000 分
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)

	// 发货 80
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-i1", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100, ShippedQty: 80,
		BatchSeq: 1, IdempotencyKey: "ship-i1",
	}, []ShipmentItem{{
		ID: "si-i1", TenantID: "chang-an", ShipmentID: "shp-i1",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 80,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("apply shipment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	applyInvoice := func(id string, qty, amount int64) error {
		tx, err := e.repo.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		err = e.repo.ApplyInvoice(ctx, tx, Invoice{
			ID: id, TenantID: "chang-an", OrderID: ord.ID, State: domain.InvoiceStateIssued,
			AmountCents: amount, Qty: qty, IdempotencyKey: "inv-" + id,
		}, []InvoiceItem{{
			ID: "ii-" + id, TenantID: "chang-an", InvoiceID: id,
			OrderItemID: item.ID, ProductID: "prod-c68", Qty: qty, AmountCents: amount, IdempotencyKey: "invitem-" + id,
		}}, "u-accounting")
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// 数量维度超限：80 可开，开 81 拒绝
	if err := applyInvoice("inv-over-qty", 81, 80000); err == nil {
		t.Fatal("invoice qty over shipped qty should be rejected")
	}
	// 金额维度超限：80×1000=80000 可开，开 80001 拒绝
	if err := applyInvoice("inv-over-amt", 80, 80001); err == nil {
		t.Fatal("invoice amount over shipped amount should be rejected")
	}
	// 合法：50 × 50000
	if err := applyInvoice("inv-ok1", 50, 50000); err != nil {
		t.Fatalf("legal invoice rejected: %v", err)
	}
	// 再开 30 × 30000（累计 80/80000 顶格）
	if err := applyInvoice("inv-ok2", 30, 30000); err != nil {
		t.Fatalf("second legal invoice rejected: %v", err)
	}
	// 顶格后再开 → 拒绝
	if err := applyInvoice("inv-over-total", 1, 1000); err == nil {
		t.Fatal("invoice beyond shipped total should be rejected")
	}
}

// L1-06 · 迁移校验：001-003 幂等可重跑（空库 + 二次执行）
func TestL106MigrationReRunnable(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	// applyMigrations 已执行一遍；再跑一遍验证幂等（IF NOT EXISTS）
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	ctx := context.Background()
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if _, err := e.conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply migration %s: %v", f, err)
		}
	}
	var n int
	if err := e.conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 25 {
		t.Fatalf("expected >=25 tables after re-run, got %d", n)
	}
}

// L1-11 · 待办检索：approval_tasks 按 assignee/kind/state（CQ-05）
func TestL111ApprovalTaskQuery(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	ord := e.createTestOrder("款到", 10)
	item := e.getOrderItem(ord.ID)
	_ = item

	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	ids := map[string]string{}
	for _, kind := range []string{"PRICE", "CHAIRMAN", "STOCK_WAREHOUSE", "PAYMENT"} {
		id, err := e.repo.CreateApprovalTask(ctx, tx, NewApprovalTask{
			TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: kind,
			AssigneeID: strPtr("u-supervisor"),
		})
		if err != nil {
			t.Fatalf("create task %s: %v", kind, err)
		}
		ids[kind] = id
	}
	// 同 kind 去重：再建 PRICE 返回既有
	id2, err := e.repo.CreateApprovalTask(ctx, tx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "ORD", Kind: "PRICE", AssigneeID: strPtr("u-supervisor"),
	})
	if err != nil || id2 != ids["PRICE"] {
		t.Fatalf("dup kind should return existing task: %v %v", id2, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 按 assignee 检索
	tasks, err := e.repo.ListApprovalTasks(ctx, "chang-an", "u-supervisor", "", "", 100)
	if err != nil || len(tasks) != 4 {
		t.Fatalf("list by assignee: %d %v", len(tasks), err)
	}
	// 按 kind 检索
	tasks, err = e.repo.ListApprovalTasks(ctx, "chang-an", "", "CHAIRMAN", "", 100)
	if err != nil || len(tasks) != 1 || tasks[0].Kind != "CHAIRMAN" {
		t.Fatalf("list by kind: %d %v", len(tasks), err)
	}
	// 按 state 检索（未决）
	tasks, err = e.repo.ListApprovalTasks(ctx, "chang-an", "", "", "PENDING", 100)
	if err != nil || len(tasks) != 4 {
		t.Fatalf("list by state: %d %v", len(tasks), err)
	}

	// 裁决 + 状态过滤
	tx2, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	defer tx2.Rollback(ctx)
	if err := e.repo.DecideTask(ctx, tx2, "chang-an", ids["PRICE"], "APPROVED", "通过", "u-supervisor", nil); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	tasks, err = e.repo.ListApprovalTasks(ctx, "chang-an", "u-supervisor", "", "APPROVED", 100)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("list approved: %d %v", len(tasks), err)
	}
	// 重复裁决被拒
	tx3, _ := e.repo.Begin(ctx)
	defer tx3.Rollback(ctx)
	if err := e.repo.DecideTask(ctx, tx3, "chang-an", ids["PRICE"], "REJECTED", "拒绝", "u-supervisor", nil); err == nil {
		t.Fatal("deciding an already-decided task should fail")
	}
	_ = tx3.Rollback(ctx)
	// 未知任务
	tx4, _ := e.repo.Begin(ctx)
	defer tx4.Rollback(ctx)
	if err := e.repo.DecideTask(ctx, tx4, "chang-an", "AT-nonexistent", "APPROVED", "x", "u-supervisor", nil); !IsNotFound(err) {
		t.Fatalf("decide unknown task should 404, got %v", err)
	}
	_ = tx4.Rollback(ctx)
}

// L1-12 · 分域事件链：状态迁移与对应事件同事务 append-only（防线 6；CQ-12）
func TestL112EventChainAppendOnly(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	ord := e.createTestOrder("款到", 10)
	item := e.getOrderItem(ord.ID)

	// 状态迁移 + 事件同事务
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := e.repo.UpdateOrderState(ctx, tx, "chang-an", ord.ID, domain.StateORDRequested,
		domain.StateORDPriceReviewing, "PriceReviewStarted", "u-supervisor", "提交审价", nil); err != nil {
		t.Fatalf("update state: %v", err)
	}
	// 事务未提交前：新事件不可见（只可见已提交的 OrderRequested = 1 条）
	var evCount int
	_ = e.conn.QueryRow(ctx, `SELECT count(*) FROM order_events WHERE order_id=$1`, ord.ID).Scan(&evCount)
	if evCount != 1 {
		t.Fatalf("uncommitted event visible before commit: count=%d (want 1)", evCount)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	events, err := e.repo.ListOrderEvents(ctx, "chang-an", ord.ID, 100)
	if err != nil || len(events) != 2 { // OrderRequested + PriceReviewStarted
		t.Fatalf("order events: %d %v", len(events), err)
	}
	if events[0].Action != "OrderRequested" || events[1].Action != "PriceReviewStarted" {
		t.Fatalf("event order mismatch: %s %s", events[0].Action, events[1].Action)
	}
	if events[1].FromState == nil || *events[1].FromState != domain.StateORDRequested ||
		events[1].ToState == nil || *events[1].ToState != domain.StateORDPriceReviewing {
		t.Fatalf("state transition not recorded: %v -> %v", events[1].FromState, events[1].ToState)
	}

	// 预订单事件链
	preKey, _ := domain.PreOrderKey("chang-an", "g1", "m1", "v1")
	po, _, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil {
		t.Fatalf("create preorder: %v", err)
	}
	tx2, _ := e.repo.Begin(ctx)
	defer tx2.Rollback(ctx)
	if err := e.repo.UpdatePreOrderState(ctx, tx2, "chang-an", po.ID, domain.StatePreCaptured,
		domain.StatePreIntentParsing, "IntentParsing", "意图识别", nil, ""); err != nil {
		t.Fatalf("update preorder: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	preEvents, err := e.repo.ListPreOrderEvents(ctx, "chang-an", po.ID)
	if err != nil || len(preEvents) != 2 {
		t.Fatalf("preorder events: %d %v", len(preEvents), err)
	}
	if preEvents[0].Action != "PreOrderCaptured" || preEvents[1].Action != "IntentParsing" {
		t.Fatalf("preorder event chain mismatch")
	}

	// 发货事件链（shipment_events）
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 10)
	tx3, _ := e.repo.Begin(ctx)
	defer tx3.Rollback(ctx)
	if err := e.repo.ApplyShipment(ctx, tx3, Shipment{
		ID: "shp-ev", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10, ShippedQty: 10,
		BatchSeq: 1, IdempotencyKey: "ship-ev",
	}, []ShipmentItem{{
		ID: "si-ev", TenantID: "chang-an", ShipmentID: "shp-ev",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("apply shipment: %v", err)
	}
	if err := tx3.Commit(ctx); err != nil {
		t.Fatalf("commit3: %v", err)
	}
	var shipEvCount int
	_ = e.conn.QueryRow(ctx, `SELECT count(*) FROM shipment_events WHERE shipment_id='shp-ev'`).Scan(&shipEvCount)
	if shipEvCount != 1 {
		t.Fatalf("shipment events: %d", shipEvCount)
	}

	// 发货事件链追加（CloseShipment）
	tx4, _ := e.repo.Begin(ctx)
	defer tx4.Rollback(ctx)
	if err := e.repo.CloseShipment(ctx, tx4, "chang-an", "shp-ev", "u-op"); err != nil {
		t.Fatalf("close shipment: %v", err)
	}
	if err := tx4.Commit(ctx); err != nil {
		t.Fatalf("commit4: %v", err)
	}
	_ = e.conn.QueryRow(ctx, `SELECT count(*) FROM shipment_events WHERE shipment_id='shp-ev'`).Scan(&shipEvCount)
	if shipEvCount != 2 {
		t.Fatalf("shipment events after close: %d", shipEvCount)
	}
}

// L1-13 · 库存聚合对账：∑发货 vs 订单量（防线 5；CQ-06/07）
func TestL113ReconcileAggregates(t *testing.T) {
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

	// 发货 60
	tx, _ := e.repo.Begin(ctx)
	defer tx.Rollback(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-r", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100, ShippedQty: 60,
		BatchSeq: 1, IdempotencyKey: "ship-r",
	}, []ShipmentItem{{
		ID: "si-r", TenantID: "chang-an", ShipmentID: "shp-r",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 60,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("apply shipment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := e.repo.ReconcileAllOrders(ctx, "chang-an")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var found *ReconcileOrder
	for i := range rows {
		if rows[i].OrderID == ord.ID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("order not in reconcile result")
	}
	if found.OrderQty != 100 || found.PreparedQty != 100 || found.ShippedQty != 60 {
		t.Fatalf("reconcile numbers: order=%d prepared=%d shipped=%d", found.OrderQty, found.PreparedQty, found.ShippedQty)
	}
	if len(found.Violations) != 0 {
		t.Fatalf("unexpected violations: %v", found.Violations)
	}
	// 负库存清单为空
	neg, err := e.repo.ReconcileInventory(ctx, "chang-an")
	if err != nil || len(neg) != 0 {
		t.Fatalf("negative inventory list: %d %v", len(neg), err)
	}
}

// L1-14 · 企微存档游标 checkpoint 原子性（seq 持久化；CQ-11；07 §2.3）
func TestL114ArchiveCursorCheckpoint(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 初始 0
	seq, err := e.repo.GetArchiveCursor(ctx, "chang-an")
	if err != nil || seq != 0 {
		t.Fatalf("initial cursor = %d, %v", seq, err)
	}
	// 推进 100
	if err := e.repo.AdvanceArchiveCursor(ctx, "chang-an", 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	seq, _ = e.repo.GetArchiveCursor(ctx, "chang-an")
	if seq != 100 {
		t.Fatalf("cursor after advance = %d, want 100", seq)
	}
	// 推进更大值 250
	if err := e.repo.AdvanceArchiveCursor(ctx, "chang-an", 250); err != nil {
		t.Fatalf("advance2: %v", err)
	}
	seq, _ = e.repo.GetArchiveCursor(ctx, "chang-an")
	if seq != 250 {
		t.Fatalf("cursor after advance2 = %d, want 250", seq)
	}
	// 回退被拒（中断重启后不丢 checkpoint）
	if err := e.repo.AdvanceArchiveCursor(ctx, "chang-an", 200); err == nil {
		t.Fatal("cursor regression should be rejected")
	}
	// 独立租户游标互不影响
	seq2, _ := e.repo.GetArchiveCursor(ctx, "zhichu")
	if seq2 != 0 {
		t.Fatalf("other tenant cursor = %d, want 0", seq2)
	}
}

var _ = pgx.ErrNoRows
