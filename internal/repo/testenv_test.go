//go:build integration

package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// ── L1 测试环境：真实 PostgreSQL 直连（docs/03-testing.html §5）──
// 通过环境变量 PILOT_TEST_PG_DSN 覆盖，默认 localhost:55432/b2b_pilot_test。

const defaultTestDSN = "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"

type testEnv struct {
	repo *Repo
	conn *pgx.Conn
	t    *testing.T
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("PILOT_TEST_PG_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	ctx := context.Background()
	repo, err := New(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable (%v) — L1 integration test skipped (degraded mode)", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	e := &testEnv{repo: repo, conn: conn, t: t}
	e.applyMigrations(t)
	e.truncateAll(t)
	return e
}

// applyMigrations 重置 schema 后按序应用全部迁移（L1-06 语义：空库可执行 + 可重跑）。
func (e *testEnv) applyMigrations(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := e.conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func (e *testEnv) truncateAll(t *testing.T) {
	t.Helper()
	_, err := e.conn.Exec(context.Background(), `
		TRUNCATE pre_order_events, pre_orders, invoice_events, invoice_items, invoices,
		         shipment_events, shipment_items, shipments, stock_prepare_events, stock_prepare_items,
		         stock_prepares, approval_comments, approval_tasks, order_events, order_items, orders,
		         inventory_items, legacy_flow_mapping, migration_batches, pgapi_entity_mappings,
		         wecom_context_bindings, wecom_archive_cursor, customers, products, warehouses, users, tenants
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// seedWorld 播种标准演示世界（与 scripts/seeddata 同源数据）。
func (e *testEnv) seedWorld() {
	if err := e.repo.SeedDefaultWorld(context.Background(), "l1-test"); err != nil {
		e.t.Fatalf("seed world: %v", err)
	}
}

// createTestOrder 快速下单（款到/非款到），返回订单。
func (e *testEnv) createTestOrder(paymentTerm string, qty int64) *Order {
	ctx := context.Background()
	key, err := domain.OrderKey("chang-an", "quick", uuidSuffix())
	if err != nil {
		e.t.Fatalf("order key: %v", err)
	}
	payload := []byte(fmt.Sprintf(`{"payment_term":%q,"run":"l1"}`, paymentTerm))
	ord, created, err := e.repo.CreateOrder(ctx, NewOrder{
		TenantID:        "chang-an",
		CustomerID:      "cust-0001",
		PaymentTerm:     paymentTerm,
		SupplierChannel: "渠道A",
		IdempotencyKey:  key,
		Payload:         payload,
		Items: []NewOrderItem{{
			ID:             "item-" + uuidSuffix()[:6],
			ProductID:      "prod-c68",
			WarehouseID:    strPtr("wh-yangyuan"),
			Quantity:       qty,
			UnitPriceCents: 1000,
		}},
	})
	if err != nil || !created {
		e.t.Fatalf("create order: created=%v err=%v", created, err)
	}
	return ord
}

// createTestPrepare 建备货审批聚合（款到路径前置）。
func (e *testEnv) createTestPrepare(orderID string, orderItemID string, warehouseID string, plannedQty int64) string {
	ctx := context.Background()
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		e.t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	spID := "STK-" + uuidSuffix()[:8]
	spKey := "sp:" + spID + ":" + uuidSuffix()
	if err := e.repo.CreateStockPrepare(ctx, tx, StockPrepare{
		ID: spID, TenantID: "chang-an", OrderID: orderID, WarehouseID: warehouseID,
		State: domain.StateSTKWarehouseConfirm, PlannedQty: plannedQty, RemainingQty: plannedQty,
		BatchSeq: 1, IdempotencyKey: spKey,
	}, []StockPrepareItem{{
		ID: "spi-" + uuidSuffix()[:6], TenantID: "chang-an", StockPrepareID: spID,
		OrderItemID: orderItemID, ProductID: "prod-c68", Qty: plannedQty,
	}}); err != nil {
		e.t.Fatalf("create stock prepare: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatalf("commit: %v", err)
	}
	return spID
}

// getOrderItems 取订单明细（首条）。
func (e *testEnv) getOrderItem(orderID string) OrderItem {
	items, err := e.repo.GetOrderItems(context.Background(), "chang-an", orderID)
	if err != nil || len(items) == 0 {
		e.t.Fatalf("get order items: %v", err)
	}
	return items[0]
}

func strPtr(s string) *string { return &s }

func (e *testEnv) close() {
	// 防挂死护栏：若某测试泄漏了事务/连接（未 rollback），pool.Close 会永久阻塞；
	// 用超时包装把「静默挂死」变成「显式失败」。
	done := make(chan struct{})
	go func() {
		e.repo.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		e.t.Errorf("testEnv.close: pgxpool.Close blocked >5s — a transaction/connection was leaked")
	}
	_ = e.conn.Close(context.Background())
}
