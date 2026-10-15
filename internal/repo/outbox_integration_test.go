//go:build integration

package repo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
)

// ── Outbox 投影事件（决策 D4；L1-15 的 repo 侧）──

func TestOutboxAppendAndConsume(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 初始无 PENDING
	evs, err := e.repo.ListPendingOutboxEvents(ctx, 100)
	if err != nil || len(evs) != 0 {
		t.Fatalf("initial pending: %v %v", evs, err)
	}

	// 业务事务追加（同事务 outbox）
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := e.repo.AppendOutboxEvent(ctx, tx, "chang-an", "prestock", "PS-1", "prestock.created",
		[]byte(`{"owner_id":"u-op"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 回滚场景：事件不残留
	tx2, _ := e.repo.Begin(ctx)
	if err := e.repo.AppendOutboxEvent(ctx, tx2, "chang-an", "prestock", "PS-2", "prestock.created",
		[]byte(`{"owner_id":"u-op"}`)); err != nil {
		t.Fatalf("append2: %v", err)
	}
	_ = tx2.Rollback(ctx)

	evs, err = e.repo.ListPendingOutboxEvents(ctx, 100)
	if err != nil || len(evs) != 1 {
		t.Fatalf("pending after append: %d %v", len(evs), err)
	}
	if evs[0].AggregateID != "PS-1" || evs[0].EventType != "prestock.created" {
		t.Fatalf("event content: %+v", evs[0])
	}

	// 幂等标记 PROCESSED；重复标记不报错
	if err := e.repo.MarkOutboxProcessed(ctx, evs[0].ID); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	if err := e.repo.MarkOutboxProcessed(ctx, evs[0].ID); err != nil {
		t.Fatalf("mark processed twice: %v", err)
	}
	evs, _ = e.repo.ListPendingOutboxEvents(ctx, 100)
	if len(evs) != 0 {
		t.Fatalf("pending after process: %d", len(evs))
	}
}

func TestOutboxContactAndDelegation(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 场景 H：客户联系人变更（事实表 + contact_upsert 事件）
	old, err := e.repo.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "li-customer")
	if err != nil || old != "" {
		t.Fatalf("first contact: old=%q err=%v", old, err)
	}
	contact, err := e.repo.GetCustomerContact(ctx, "chang-an", "cust-0001")
	if err != nil || contact != "li-customer" {
		t.Fatalf("get contact: %q %v", contact, err)
	}
	old, err = e.repo.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "wang-customer")
	if err != nil || old != "li-customer" {
		t.Fatalf("rotate contact: old=%q err=%v", old, err)
	}
	// D-006 后 customer_id 为 pgAPI record_id 外部引用，本地 customer_contacts 不再校验本地 customers。
	if _, err := e.repo.SetCustomerContactTx(ctx, "chang-an", "cust-nope", "x"); err != nil {
		t.Fatalf("external customer contact update should succeed: %v", err)
	}
	if _, err := e.repo.GetCustomerContact(ctx, "chang-an", "cust-other"); !IsNotFound(err) {
		t.Fatalf("unknown contact should 404: %v", err)
	}

	// 场景 G：代办授权 grant/revoke 事件
	exp := time.Now().Add(72 * time.Hour)
	if err := e.repo.GrantDelegationTx(ctx, "chang-an", "prestock", "PS-8801", "li-na", exp); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := e.repo.RevokeDelegationTx(ctx, "chang-an", "prestock", "PS-8801", "li-na"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	evs, err := e.repo.ListPendingOutboxEvents(ctx, 100)
	// D-006：cust-nope 是外部 record_id，不再被本地 customers FK 拒绝，
	// 因此也会产生第 3 条 customer.contact_upsert；加上 grant/revoke 共 5 条。
	if err != nil || len(evs) != 5 {
		t.Fatalf("pending: %d %v", len(evs), err)
	}
	types := map[string]bool{}
	for _, ev := range evs {
		types[ev.EventType] = true
	}
	if !types["customer.contact_upsert"] || !types["delegation.grant"] || !types["delegation.revoke"] {
		t.Fatalf("event types: %v", types)
	}
	// grant payload 含 expires_at
	for _, ev := range evs {
		if ev.EventType == "delegation.grant" {
			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload["expires_at"] == nil {
				t.Fatalf("grant payload: %v %v", payload, err)
			}
		}
	}
	// MarkOutboxFailed 幂等
	if err := e.repo.MarkOutboxFailed(ctx, evs[0].ID, "inject"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
}

// 业务写入点：备货/发货事务自动追加投影事件
func TestOutboxBusinessWritePoints(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	// 状态推进到 STK_STOCK_REVIEWING（ReserveStockTx 前置）
	for _, st := range []struct{ from, to string }{
		{domain.StateORDRequested, domain.StateORDPriceReviewing},
		{domain.StateORDPriceReviewing, domain.StateORDPriceApproved},
		{domain.StateORDPriceApproved, domain.StateORDContractGenerate},
		{domain.StateORDContractGenerate, domain.StateORDContracted},
		{domain.StateORDContracted, domain.StateSTKReadyForStock},
		{domain.StateSTKReadyForStock, domain.StateSTKStockReviewing},
	} {
		if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, st.from, st.to, "x", "u", "r", nil); err != nil {
			t.Fatalf("advance %s->%s: %v", st.from, st.to, err)
		}
	}

	// ReserveStockTx → prestock.created
	if err := e.repo.ReserveStockTx(ctx, StockPrepare{
		ID: "STK-ob1", TenantID: "chang-an", OrderID: ord.ID, WarehouseID: "wh-yangyuan",
		State: domain.StateSTKWarehouseConfirm, PlannedQty: 100, RemainingQty: 100,
		BatchSeq: 1, IdempotencyKey: "sp-ob-1",
	}, []StockPrepareItem{{
		ID: "spi-ob1", TenantID: "chang-an", StockPrepareID: "STK-ob1",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, domain.StateSTKStockReviewing, domain.StateSTKWarehouseConfirm, "u-op", "商务备货", nil); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 状态推进到 SHP_SHIP_ASSIGNING（ShipAndUpdateOrderTx 前置）
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKWarehouseConfirm,
		domain.StateSTKShipOrderIssued, "x", "u", "r", nil); err != nil {
		t.Fatalf("advance ship order: %v", err)
	}
	if err := e.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKShipOrderIssued,
		domain.StateSHPShipAssigning, "x", "u", "r", nil); err != nil {
		t.Fatalf("advance ship assign: %v", err)
	}
	// ShipAndUpdateOrderTx → shipment.created
	if err := e.repo.ShipAndUpdateOrderTx(ctx, Shipment{
		ID: "SHP-ob1", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: "STK-ob1",
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-ob-1",
	}, []ShipmentItem{{
		ID: "si-ob1", TenantID: "chang-an", ShipmentID: "SHP-ob1",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 100,
	}}, domain.StateSHPShipAssigning, domain.StateSHPShipPed, "u-wh-yangyuan", "实发", nil); err != nil {
		t.Fatalf("ship: %v", err)
	}
	evs, err := e.repo.ListPendingOutboxEvents(ctx, 100)
	if err != nil || len(evs) != 2 {
		t.Fatalf("business events: %d %v", len(evs), err)
	}
	if evs[0].EventType != "prestock.created" || evs[1].EventType != "shipment.created" {
		t.Fatalf("event order/types: %s %s", evs[0].EventType, evs[1].EventType)
	}
	var payload map[string]any
	_ = json.Unmarshal(evs[1].Payload, &payload)
	if payload["warehouse_operator_id"] != "u-wh-yangyuan" {
		t.Fatalf("shipment.created payload: %v", payload)
	}
}

// Requeue 失败重放 + 幂等边界
func TestOutboxRequeueAndEdges(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 追加 2 个事件：1 个标记 FAILED，1 个保持 PENDING
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.AppendOutboxEvent(ctx, tx, "chang-an", "prestock", "PS-r1", "prestock.created", []byte(`{"owner_id":"u-op"}`)); err != nil {
		t.Fatalf("append1: %v", err)
	}
	if err := e.repo.AppendOutboxEvent(ctx, tx, "chang-an", "prestock", "PS-r2", "prestock.created", []byte(`{"owner_id":"u-op"}`)); err != nil {
		t.Fatalf("append2: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	evs, _ := e.repo.ListPendingOutboxEvents(ctx, 10)
	require.Len(t, evs, 2)
	require.NoError(t, e.repo.MarkOutboxFailed(ctx, evs[0].ID, "inject"))
	// FAILED 不再出现在 PENDING
	evs2, _ := e.repo.ListPendingOutboxEvents(ctx, 10)
	require.Len(t, evs2, 1)
	// Requeue：FAILED → PENDING
	n, err := e.repo.RequeueFailedOutboxEvents(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	evs3, _ := e.repo.ListPendingOutboxEvents(ctx, 10)
	require.Len(t, evs3, 2)
	// 再 Requeue 无 FAILED → 0
	n, err = e.repo.RequeueFailedOutboxEvents(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
}

// Grant/Revoke 的 commit 前错误注入（触发器：outbox_events INSERT 失败 → 全量回滚）
func TestOutboxDelegationRollback(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	if _, err := e.conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_outbox() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected: outbox append failure'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_fail_ob BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_outbox();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer e.conn.Exec(ctx, `DROP TRIGGER IF EXISTS trg_fail_ob ON outbox_events; DROP FUNCTION IF EXISTS fail_outbox();`)

	// GrantDelegationTx 中途失败 → 事件不残留
	if err := e.repo.GrantDelegationTx(ctx, "chang-an", "prestock", "PS-1", "li-na", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("injected outbox failure should error")
	}
	evs, _ := e.repo.ListPendingOutboxEvents(ctx, 10)
	require.Empty(t, evs)
	// SetCustomerContactTx 同样回滚（customer_contacts 不残留）
	if _, err := e.repo.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "li"); err == nil {
		t.Fatal("injected outbox failure should error on contact")
	}
	if _, err := e.repo.GetCustomerContact(ctx, "chang-an", "cust-0001"); !IsNotFound(err) {
		t.Fatalf("contact should be rolled back: %v", err)
	}
}
