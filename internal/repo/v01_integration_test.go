//go:build integration

package repo

import (
	"context"
	"testing"
	"time"

	"b2b_pilot_v3/internal/domain"
)

// ── 0.1 发布版 L1 集成测试（docs/v0.1/09-testing §3 N-05/N-09/N-10 + 11-migration V1）──
// 真 PG：多单合并开票关联/三金额、超发拦截、仓库群回单机器人登记、同步批次幂等。

// N-05 · 多单合并开票（A4）：发票关联订单 1、2 → 追加关联 3 → 三金额分展 →
// 未开票额度保留；关联守恒（超出未开票额度拒绝）；重复追加幂等。
func TestV01N05MergeInvoiceLinks(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	mk := func(id string) (string, string) {
		ord := e.createTestOrder("款到", 100)
		item := e.getOrderItem(ord.ID)
		spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)
		tx, _ := e.repo.Begin(ctx)
		if err := e.repo.ApplyShipment(ctx, tx, Shipment{
			ID: "shp-" + id, TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
			WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
			ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-" + id,
		}, []ShipmentItem{{
			ID: "si-" + id, TenantID: "chang-an", ShipmentID: "shp-" + id,
			OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
		}}, "u-wh-yangyuan"); err != nil {
			t.Fatalf("ship %s: %v", id, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit ship %s: %v", id, err)
		}
		return ord.ID, item.ID
	}
	ord1, item1 := mk("n05a")
	ord2, _ := mk("n05b")
	ord3, _ := mk("n05c")

	// 发票 1 关联订单 1（主关联）
	inv := Invoice{ID: "INV-n05", TenantID: "chang-an", OrderID: ord1, State: domain.InvoiceStateIssued,
		AmountCents: 100000, Qty: 100, IdempotencyKey: "inv:n05"}
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyInvoice(ctx, tx, inv, []InvoiceItem{{
		ID: "ii-n05-1", TenantID: "chang-an", InvoiceID: "INV-n05", OrderItemID: item1,
		ProductID: "prod-c68", Qty: 100, AmountCents: 100000, IdempotencyKey: "inv:n05:1",
	}}, "u-accounting"); err != nil {
		t.Fatalf("apply invoice: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit invoice: %v", err)
	}

	// 追加关联订单 2（漏了可补）
	created, err := e.repo.LinkInvoiceOrder(ctx, "chang-an", "INV-n05", ord2, 100000, 100)
	if err != nil || !created {
		t.Fatalf("link order2: created=%v err=%v", created, err)
	}
	// 追加关联订单 3（可补关联第 3 笔）
	created, err = e.repo.LinkInvoiceOrder(ctx, "chang-an", "INV-n05", ord3, 100000, 100)
	if err != nil || !created {
		t.Fatalf("link order3: created=%v err=%v", created, err)
	}
	// 重复追加 → 幂等 created=false
	created, err = e.repo.LinkInvoiceOrder(ctx, "chang-an", "INV-n05", ord2, 100000, 100)
	if err != nil || created {
		t.Fatalf("dup link: created=%v err=%v", created, err)
	}
	// 已关联订单重复追加 → 幂等 created=false（先于守恒；不重复计额）
	if created, err := e.repo.LinkInvoiceOrder(ctx, "chang-an", "INV-n05", ord1, 1, 1); err != nil || created {
		t.Fatalf("dup primary link: created=%v err=%v", created, err)
	}
	// 关联守恒：第二张发票重复开票订单 1（links 已全额）→ 拒绝（订单级守恒）
	inv2 := Invoice{ID: "INV-n05b", TenantID: "chang-an", OrderID: ord1, State: domain.InvoiceStateIssued,
		AmountCents: 1, Qty: 1, IdempotencyKey: "inv:n05b"}
	tx2, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyInvoice(ctx, tx2, inv2, []InvoiceItem{{
		ID: "ii-n05b", TenantID: "chang-an", InvoiceID: "INV-n05b", OrderItemID: item1,
		ProductID: "prod-c68", Qty: 1, AmountCents: 1, IdempotencyKey: "inv:n05b:1",
	}}, "u-accounting"); !domain.IsConservation(err) {
		t.Fatalf("over-link should be conservation-rejected, got %v", err)
	}
	tx2.Rollback(ctx)

	// 三金额分展（A4）：订单 2 发货 100000、开票 100000、未开票 0
	amt, err := e.repo.OrderThreeAmounts(ctx, "chang-an", ord2)
	if err != nil {
		t.Fatalf("three amounts: %v", err)
	}
	if amt.ShippedCents != 100000 || amt.InvoicedCents != 100000 || amt.UninvoicedCents != 0 {
		t.Fatalf("order2 amounts: %+v", amt)
	}
	// 回款登记 → 收款金额快照（发票 received_cents）
	if err := e.repo.TrackPaymentTx(ctx, "chang-an", ord2, 100000); err != nil {
		t.Fatalf("track payment: %v", err)
	}
	invGot, err := e.repo.GetInvoice(ctx, "chang-an", "INV-n05")
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if invGot.ReceivedCents != 100000 {
		t.Fatalf("invoice received_cents = %d, want 100000", invGot.ReceivedCents)
	}
	links, err := e.repo.ListInvoiceLinks(ctx, "chang-an", "INV-n05")
	if err != nil || len(links) != 3 {
		t.Fatalf("links = %d err=%v, want 3", len(links), err)
	}
	// 收款金额分展（PaymentTracked 事件聚合；直接注入事件测试聚合口径，状态机由工作流测试覆盖）
	if _, err := e.conn.Exec(ctx, `
		INSERT INTO order_events (tenant_id, order_id, stage_code, action, from_state, to_state, reason, detail, operator_id)
		VALUES ($1,$2,'INV','PaymentTracked','INV_INVOICED','INV_PAYMENT_TRACKING','回款 ¥100000', '{"amount":100000}', 'u-accounting')`,
		"chang-an", ord2); err != nil {
		t.Fatalf("payment event: %v", err)
	}
	amt2, err := e.repo.OrderThreeAmounts(ctx, "chang-an", ord2)
	if err != nil {
		t.Fatalf("three amounts 2: %v", err)
	}
	if amt2.ReceivedCents != 100000 {
		t.Fatalf("order2 received = %d, want 100000", amt2.ReceivedCents)
	}
}

// N-09 · 超发拦截（CQ-07）：备货守恒（∑发货 ≤ ∑备货）+ CHECK(shipped_qty <= planned_qty)。
func TestV01N09OvershipIntercepted(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 5)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 5)

	// 输入 6（应发 5）→ 守恒拒绝
	tx, _ := e.repo.Begin(ctx)
	err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-over", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 5,
		ShippedQty: 6, BatchSeq: 1, IdempotencyKey: "ship-over",
	}, []ShipmentItem{{
		ID: "si-over", TenantID: "chang-an", ShipmentID: "shp-over",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 6,
	}}, "u-wh-yangyuan")
	if !domain.IsConservation(err) {
		t.Fatalf("input 6 > 应发 5 should be rejected, got %v", err)
	}
	tx.Rollback(ctx)

	// 修正 5 → 通过
	tx2, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx2, Shipment{
		ID: "shp-ok5", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 5,
		ShippedQty: 5, BatchSeq: 1, IdempotencyKey: "ship-ok5",
	}, []ShipmentItem{{
		ID: "si-ok5", TenantID: "chang-an", ShipmentID: "shp-ok5",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 5,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("corrected 5 should pass: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// CHECK 约束兜底：shipped_qty > planned_qty 直接拒绝
	_, err = e.conn.Exec(ctx, `INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id,
		state, planned_qty, shipped_qty, batch_seq, idempotency_key)
		VALUES ('shp-check',$1,$2,$3,'wh-yangyuan','OPEN',5,6,1,'ship-check')`, "chang-an", ord.ID, spID)
	if err == nil {
		t.Fatal("CHECK(shipped_qty <= planned_qty) should reject overship")
	}
}

// N-10 · 仓库群回单机器人（A6）：仓库群绑定 → 凭证 v3 只追加（robot 登记不覆盖）。
func TestV01N10WarehouseGroupReceiptRobot(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 10)
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-n10", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10,
		ShippedQty: 10, BatchSeq: 1, IdempotencyKey: "ship-n10",
	}, []ShipmentItem{{
		ID: "si-n10", TenantID: "chang-an", ShipmentID: "shp-n10",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 仓库群绑定（seedWorld 已种 group-wh-yangyuan）
	tenantID, wh, err := e.repo.LookupWarehouseGroupBinding(ctx, "group-wh-yangyuan")
	if err != nil || tenantID != "chang-an" || wh != "wh-yangyuan" {
		t.Fatalf("warehouse group binding: %s/%s err=%v", tenantID, wh, err)
	}
	if _, _, err := e.repo.LookupWarehouseGroupBinding(ctx, "group-unknown"); !IsNotFound(err) {
		t.Fatalf("unknown group should be not-found, got %v", err)
	}
	// 机器人登记 v1 → v2 → v3（只追加）
	for i := 1; i <= 3; i++ {
		ver, err := e.repo.AppendEvidenceMeta(ctx, "chang-an", "shp-n10", "robot",
			"file://receipt-"+itoaStr(i)+".jpg", "hash-"+itoaStr(i), EvidenceMeta{Kind: "RECEIPT", Source: "robot"})
		if err != nil {
			t.Fatalf("append v%d: %v", i, err)
		}
		if ver != i {
			t.Fatalf("version = %d, want %d", ver, i)
		}
	}
	vs, err := e.repo.ListEvidenceVersions(ctx, "chang-an", "shp-n10")
	if err != nil || len(vs) != 3 {
		t.Fatalf("versions = %d err=%v, want 3 (v1/v2 保留)", len(vs), err)
	}
	if vs[2].Kind != "RECEIPT" || vs[2].Source != "robot" || vs[2].UploaderID != "robot" {
		t.Fatalf("v3 meta wrong: %+v", vs[2])
	}
}

// A1 · 收货地址落库（order_items.receive_address）。
func TestV01ReceiveAddressStored(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	key, _ := domain.OrderKey("chang-an", "addr", time.Now().Format("20060102150405.000000000"))
	ord, created, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到",
		IdempotencyKey: key, Payload: []byte(`{}`),
		Items: []NewOrderItem{{ID: "item-addr", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 5, UnitPriceCents: 1000, ReceiveAddress: "湖南岳阳 · 云溪区"}},
	})
	if err != nil || !created {
		t.Fatalf("create order: created=%v err=%v", created, err)
	}
	items, err := e.repo.GetOrderItems(ctx, "chang-an", ord.ID)
	if err != nil || len(items) != 1 || items[0].ReceiveAddress != "湖南岳阳 · 云溪区" {
		t.Fatalf("receive address: %+v err=%v", items, err)
	}
}

// N-01 · 守门员确认事件（A3）：append-only + 内容指纹幂等。
func TestV01GatekeeperEventIdempotent(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)
	detail := MarshalJSONDetail(map[string]any{"content_hash": "h1", "group_id": "group-demo"})
	id1, already, err := e.repo.AppendGatekeeperConfirmation(ctx, "chang-an", ord.ID, "h1", detail, "u-sales")
	if err != nil || already {
		t.Fatalf("first confirm: already=%v err=%v", already, err)
	}
	id2, already, err := e.repo.AppendGatekeeperConfirmation(ctx, "chang-an", ord.ID, "h1", detail, "u-sales")
	if err != nil || !already || id1 != id2 {
		t.Fatalf("dup confirm: id1=%d id2=%d already=%v err=%v", id1, id2, already, err)
	}
	// 不同内容指纹 → 新事件
	_, already, err = e.repo.AppendGatekeeperConfirmation(ctx, "chang-an", ord.ID, "h2", detail, "u-sales")
	if err != nil || already {
		t.Fatalf("different hash: already=%v err=%v", already, err)
	}
}

// 11-migration V1 · 同步批次幂等（migration_batches 锚点：同批次重跑无重复）。
func TestV01SyncBatchIdempotent(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	orders := []SyncOrder{{
		OrderID: "ORD-SYNC-1", CustomerID: "cust-0001", State: "已开单", PaymentTerm: "款到",
		SupplierChannel: "渠道A", IdempotencyKey: "legacy:FLOW-SYNC-1", PayloadHash: "h", Payload: []byte(`{}`),
		LegacyFlowID: "FLOW-SYNC-1", EventAction: "OrderRequested", EventReason: "飞书同步",
		EventOperator: "u-sync", EventTime: "2026-08-01T10:00:00+08:00",
		LegacyInstanceID: "INST-SYNC-1", LegacyTaskID: "TASK-SYNC-1", StageCode: "ORD",
	}}
	applied, err := e.repo.ApplySyncBatch(ctx, "chang-an", "feishu_orders", "checksum-1", orders)
	if err != nil || !applied {
		t.Fatalf("apply batch: applied=%v err=%v", applied, err)
	}
	// 同批次重跑 → 跳过
	applied, err = e.repo.ApplySyncBatch(ctx, "chang-an", "feishu_orders", "checksum-1", orders)
	if err != nil || applied {
		t.Fatalf("replay: applied=%v err=%v", applied, err)
	}
	// 快照 upsert：同 flow_id 新状态 → 更新（不新增行）
	orders[0].State = "已发货"
	applied, err = e.repo.ApplySyncBatch(ctx, "chang-an", "feishu_orders", "checksum-2", orders)
	if err != nil || !applied {
		t.Fatalf("apply batch 2: applied=%v err=%v", applied, err)
	}
	got, err := e.repo.GetOrderByIdempotencyKey(ctx, "chang-an", "legacy:FLOW-SYNC-1")
	if err != nil || got.State != "已发货" {
		t.Fatalf("upsert state: %+v err=%v", got, err)
	}
	// 事件链搬运（append-only）
	evs, err := e.repo.ListOrderEvents(ctx, "chang-an", "ORD-SYNC-1", 100)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %d err=%v, want 2 (两个批次各一条)", len(evs), err)
	}
}

// itoaStr 简单 int → string（测试用）。
func itoaStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// 仓储边界路径（0.1 新增方法的错误分支；L1 真实 PG）
func TestV01RepoEdgePaths(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	ord := e.createTestOrder("款到", 10)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 10)

	// GetStockPrepare 成功 + 不存在
	sp, err := e.repo.GetStockPrepare(ctx, "chang-an", spID)
	if err != nil || sp.WarehouseID != "wh-yangyuan" {
		t.Fatalf("get stock prepare: %+v err=%v", sp, err)
	}
	if _, err := e.repo.GetStockPrepare(ctx, "chang-an", "STK-nope"); !IsNotFound(err) {
		t.Fatalf("missing stock prepare: %v", err)
	}

	// 发货后：ShipmentShippedAmountCents 成功 + 不存在
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, Shipment{
		ID: "shp-edge", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 10,
		ShippedQty: 10, BatchSeq: 1, IdempotencyKey: "ship-edge",
	}, []ShipmentItem{{
		ID: "si-edge", TenantID: "chang-an", ShipmentID: "shp-edge",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 10,
	}}, "u-wh-yangyuan"); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	amt, err := e.repo.ShipmentShippedAmountCents(ctx, "chang-an", "shp-edge")
	if err != nil || amt != 10000 {
		t.Fatalf("shipped amount = %d err=%v, want 10000", amt, err)
	}
	// 不存在的发货单 → 0（无明细）
	amt0, err := e.repo.ShipmentShippedAmountCents(ctx, "chang-an", "shp-nope")
	if err != nil || amt0 != 0 {
		t.Fatalf("missing shipment amount = %d err=%v, want 0", amt0, err)
	}

	// OrderThreeAmounts：不存在订单 → ErrNotFound
	if _, err := e.repo.OrderThreeAmounts(ctx, "chang-an", "ORD-nope"); !IsNotFound(err) {
		t.Fatalf("missing order amounts: %v", err)
	}

	// 绑定存在但 kind 非仓库群 → ErrNotFound（JSONB 保证合法 JSON，解析错误分支为防御性不可达）
	if err := e.repo.SeedWecomBinding(ctx, "chang-an", "", "group-cust", []byte(`{"kind":"customer_group","owner":"u-sales"}`)); err != nil {
		t.Fatalf("seed customer binding: %v", err)
	}
	if _, _, err := e.repo.LookupWarehouseGroupBinding(ctx, "group-cust"); !IsNotFound(err) {
		t.Fatalf("customer group should be not-found: %v", err)
	}

	// 同步批次：BatchApplied 查询（V1）
	if ok, err := e.repo.BatchApplied(ctx, "chang-an", "src-x", "cs-1"); err != nil || ok {
		t.Fatalf("batch not yet applied: ok=%v err=%v", ok, err)
	}
	if _, err := e.repo.ApplySyncBatch(ctx, "chang-an", "src-x", "cs-1", []SyncOrder{{
		OrderID: "ORD-EDGE", CustomerID: "cust-0001", State: "已开单", PaymentTerm: "款到",
		IdempotencyKey: "legacy:FLOW-EDGE", PayloadHash: "h", Payload: []byte(`{}`),
		LegacyFlowID: "FLOW-EDGE",
	}}); err != nil {
		t.Fatalf("apply sync: %v", err)
	}
	if ok, err := e.repo.BatchApplied(ctx, "chang-an", "src-x", "cs-1"); err != nil || !ok {
		t.Fatalf("batch applied check: ok=%v err=%v", ok, err)
	}

	// AppendEvidence（自开事务封装）错误路径：发货单不存在
	if _, err := e.repo.AppendEvidence(ctx, "chang-an", "shp-nope", "u-x", "u", "h"); err == nil {
		t.Fatal("append evidence to missing shipment should fail")
	}
}

// M1 恢复路径可用性（0.1 暂缓不删除）：库存锁/扣减函数保留并直接可工作（0.2 库存管理恢复用）。
func TestV01InventoryDormantPath(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	tx, _ := e.repo.Begin(ctx)
	defer tx.Rollback(ctx)
	// LockInventory：行锁读取
	inv, err := e.repo.LockInventory(ctx, tx, "chang-an", "wh-yangyuan", "prod-c68")
	if err != nil || inv.Qty != 100 {
		t.Fatalf("lock inventory: %+v err=%v", inv, err)
	}
	// DeductInventoryTx：扣减守恒 + CHECK
	if err := e.repo.DeductInventoryTx(ctx, tx, "chang-an", "wh-yangyuan", "prod-c68", 30); err != nil {
		t.Fatalf("deduct: %v", err)
	}
	// 超扣 → 守恒拒绝（LockInventory 缺库存 → ErrNotFound）
	if err := e.repo.DeductInventoryTx(ctx, tx, "chang-an", "wh-nope", "prod-c68", 1); !IsNotFound(err) {
		t.Fatalf("missing inventory deduct: %v", err)
	}
	// 超量扣减 → 守恒拒绝
	if err := e.repo.DeductInventoryTx(ctx, tx, "chang-an", "wh-yangyuan", "prod-c68", 999); err == nil {
		t.Fatal("over-deduct should fail conservation")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if err != nil || got.Qty != 70 {
		t.Fatalf("after deduct = %+v err=%v, want 70", got, err)
	}
}

// INV-01 收敛：发票号重复登记检查（真库）。
func TestV01InvoiceNoExists(t *testing.T) {
	e := newTestEnv(t)
	ctx := t.Context()
	if err := e.repo.SeedTenant(ctx, "chang-an", "长安仁恒"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ok, err := e.repo.InvoiceNoExists(ctx, "chang-an", "NO-NOT-EXIST")
	if err != nil || ok {
		t.Fatalf("not exist: %v %v", ok, err)
	}
	// 先建订单与发货单，再登记发票凭证
	ord, _, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到",
		IdempotencyKey: "order:chang-an:inv-dup-1",
		Payload:        []byte("{}"),
		Items:          []NewOrderItem{{ID: "item-1", ProductID: "prod-c68", Quantity: 1, UnitPriceCents: 100}},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := e.repo.AppendEvidenceMeta(ctx, "chang-an", "SHP-inv-dup", "u-accounting",
		"file://inv.pdf", "h1", EvidenceMeta{Kind: "INVOICE", InvoiceNo: "NO-DUP-1", AmountCents: 100}); err == nil {
		t.Fatal("append evidence on missing shipment must fail")
	}
	_ = ord
	ok, err = e.repo.InvoiceNoExists(ctx, "chang-an", "NO-DUP-1")
	if err != nil {
		t.Fatalf("check exists: %v", err)
	}
	if ok {
		t.Fatal("should not exist before evidence row")
	}
}

// UI-02：订单发货单列表（空/过滤）。
func TestV01ListShipmentsByOrder(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	ctx := t.Context()
	if err := e.repo.SeedTenant(ctx, "chang-an", "长安仁恒"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ord, _, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到",
		IdempotencyKey: "order:chang-an:ship-list-1", Payload: []byte("{}"),
		Items: []NewOrderItem{{ID: "item-1", ProductID: "prod-c68", Quantity: 1, UnitPriceCents: 100}},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	items, err := e.repo.ListShipmentsByOrder(ctx, "chang-an", ord.ID)
	if err != nil {
		t.Fatalf("list shipments: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("want empty shipments, got %d", len(items))
	}
	// 非空分支：插入一条发货单后应列出
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO warehouses (id, tenant_id, name) VALUES ('wh-yangyuan','chang-an','阳原仓') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO stock_prepares (id, tenant_id, order_id, warehouse_id, state, planned_qty, remaining_qty, batch_seq, split_flag, idempotency_key)
		 VALUES ('STK-shiplist-1','chang-an',$1,'wh-yangyuan','STK_WAREHOUSE_CONFIRM',1,1,1,FALSE,'sp:shiplist-1')`, []any{ord.ID}},
		{`INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, shipped_qty, batch_seq, split_flag, remaining_quantity, idempotency_key)
		 VALUES ('SHP-shiplist-1','chang-an',$1,'STK-shiplist-1','wh-yangyuan','OPEN',1,0,1,FALSE,1,'shp:shiplist-1')`, []any{ord.ID}},
	} {
		if _, err := e.conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed stmt: %v", err)
		}
	}
	items, err = e.repo.ListShipmentsByOrder(ctx, "chang-an", ord.ID)
	if err != nil {
		t.Fatalf("list shipments: %v", err)
	}
	if len(items) != 1 || items[0].ID != "SHP-shiplist-1" {
		t.Fatalf("want one shipment, got %+v", items)
	}
}

// D-04 收敛：回单凭证登记自动闭合「限时放行补齐回单」待办（例外链闭合）。
func TestV01ReceiptClosesFollowupTask(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	ctx := t.Context()
	if err := e.repo.SeedTenant(ctx, "chang-an", "长安仁恒"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ord, _, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "非款到",
		IdempotencyKey: "order:chang-an:followup-1", Payload: []byte("{}"),
		Items: []NewOrderItem{{ID: "item-1", ProductID: "prod-c68", Quantity: 1, UnitPriceCents: 100}},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO warehouses (id, tenant_id, name) VALUES ('wh-yangyuan','chang-an','阳原仓') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO stock_prepares (id, tenant_id, order_id, warehouse_id, state, planned_qty, remaining_qty, batch_seq, split_flag, idempotency_key)
		 VALUES ('STK-followup-1','chang-an',$1,'wh-yangyuan','STK_WAREHOUSE_CONFIRM',1,1,1,FALSE,'sp:followup-1')`, []any{ord.ID}},
		{`INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, shipped_qty, batch_seq, split_flag, remaining_quantity, idempotency_key)
		 VALUES ('SHP-followup-1','chang-an',$1,'STK-followup-1','wh-yangyuan','SHIPPED',1,1,1,FALSE,0,'shp:followup-1')`, []any{ord.ID}},
	}
	for _, stmt := range stmts {
		if _, err := e.conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed stmt: %v", err)
		}
	}
	if err := e.repo.SeedUser(ctx, "chang-an", "u-sales", "销售", "SALES", "wecom_sales"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	sales := "u-sales"
	taskID, err := e.repo.CreateApprovalTaskTx(ctx, NewApprovalTask{
		TenantID: "chang-an", OrderID: ord.ID, StageCode: "INV", Kind: "RECEIPT_FOLLOWUP", AssigneeID: &sales,
	})
	if err != nil {
		t.Fatalf("create followup: %v", err)
	}
	if _, err := e.repo.AppendEvidenceMeta(ctx, "chang-an", "SHP-followup-1", "robot",
		"file://receipt.jpg", "h1", EvidenceMeta{Kind: "RECEIPT", Source: "robot"}); err != nil {
		t.Fatalf("append receipt: %v", err)
	}
	task, err := e.repo.GetApprovalTask(ctx, "chang-an", taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != "APPROVED" || (task.Verdict == nil || *task.Verdict != "RECEIPT_PROVIDED") {
		t.Fatalf("followup not closed: state=%s verdict=%v", task.State, task.Verdict)
	}
}
