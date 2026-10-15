//go:build integration

package repo

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"b2b_pilot_v3/internal/domain"
)

// L1-16 · 凭证链只追加 + 版本化 + 回放（CQ-15，EP-28-N3/EP-40/EP-50）
func TestL116EvidenceAppendOnly(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)
	shp := seedShipment(e, ord.ID, spID, 40)

	// 正例 EP-50-P1：追加 v1/v2/v3，版本递增
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	v1, err := e.repo.AppendEvidenceTx(ctx, tx, "chang-an", shp.ID, "u-contact", "s3://e/v1.png", "h1", EvidenceMeta{})
	if err != nil || v1 != 1 {
		t.Fatalf("append v1 = %d, %v", v1, err)
	}
	v2, err := e.repo.AppendEvidenceTx(ctx, tx, "chang-an", shp.ID, "u-contact", "s3://e/v2.png", "h2", EvidenceMeta{})
	if err != nil || v2 != 2 {
		t.Fatalf("append v2 = %d, %v", v2, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 回放（EP-50-P2）：全版本可见且有序
	vs, err := e.repo.ListEvidenceVersions(ctx, "chang-an", shp.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vs) != 2 || vs[0].Version != 1 || vs[1].Version != 2 || vs[1].FileHash != "h2" {
		t.Fatalf("replay mismatch: %+v", vs)
	}

	// 负例 EP-50-N3：shipment 不存在拒绝
	tx2, _ := e.repo.Begin(ctx)
	defer tx2.Rollback(ctx)
	if _, err := e.repo.AppendEvidenceTx(ctx, tx2, "chang-an", "shp-bogus", "u-contact", "u", "h", EvidenceMeta{}); err == nil {
		t.Fatal("append to unknown shipment should fail")
	}

	// 负例 EP-40-N3/EP-50-N1：覆盖旧版本被拒绝（无 UPDATE/DELETE 路径，同版本 PK 冲突）
	tx3, _ := e.repo.Begin(ctx)
	defer tx3.Rollback(ctx)
	if _, err := e.repo.AppendEvidenceTx(ctx, tx3, "chang-an", shp.ID, "u-x", "u", "h", EvidenceMeta{}); err != nil {
		t.Fatalf("append v3: %v", err)
	}
	// 手工尝试覆盖 v1 → PK 冲突（只追加防线）
	if _, err := e.conn.Exec(ctx,
		`INSERT INTO evidence_versions (tenant_id, shipment_id, version, uploader_id, file_url, file_hash)
		 VALUES ($1,$2,1,'u-x','u','h')`, "chang-an", shp.ID); err == nil {
		t.Fatal("overwrite v1 should violate PK")
	}
	// 篡改路径不存在：UPDATE/DELETE 在代码层无方法（编译期保证）；SQL 层允许但业务不暴露
	// 跨租户隔离：其他租户看不到
	if vs, _ := e.repo.ListEvidenceVersions(ctx, "other-tenant", shp.ID); len(vs) != 0 {
		t.Fatal("cross-tenant evidence leak")
	}
}

// seedShipment 直接造一个 OPEN 发货单（evidence 关联用）。
func seedShipment(e *testEnv, orderID, spID string, plannedQty int64) *Shipment {
	ctx := context.Background()
	shp := &Shipment{
		ID: "SHP-" + uuidSuffix()[:8], TenantID: "chang-an", OrderID: orderID,
		StockPrepareID: spID, WarehouseID: "wh-yangyuan", State: domain.ShipmentStateOpen,
		PlannedQty: plannedQty, IdempotencyKey: "shp:" + uuidSuffix(),
	}
	if _, err := e.conn.Exec(ctx, `INSERT INTO shipments
		(id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		shp.ID, shp.TenantID, shp.OrderID, shp.StockPrepareID, shp.WarehouseID,
		string(shp.State), shp.PlannedQty, shp.IdempotencyKey); err != nil {
		e.t.Fatalf("seed shipment: %v", err)
	}
	return shp
}

// L1-16b · 并发追加版本唯一（行锁串行化，CQ-14/EP-50 并发维度）
func TestL116bEvidenceConcurrentAppend(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)
	shp := seedShipment(e, ord.ID, spID, 40)

	const workers = 6
	var wg sync.WaitGroup
	vers := make([]int, workers)
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
			v, err := e.repo.AppendEvidenceTx(ctx, tx, "chang-an", shp.ID, "u-contact", fmt.Sprintf("s3://e/v%d", n), fmt.Sprintf("h%d", n), EvidenceMeta{})
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			vers[n] = v
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	seen := map[int]bool{}
	for _, v := range vers {
		if v < 1 || v > workers {
			t.Fatalf("version out of range: %d", v)
		}
		seen[v] = true
	}
	if len(seen) != workers {
		t.Fatalf("duplicate versions: %v (want %d unique)", vers, workers)
	}
}
