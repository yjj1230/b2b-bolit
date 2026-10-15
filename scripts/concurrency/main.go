// scripts/concurrency —— 并发守恒压测（L3-01：并发发货不超备货）
// N 个 goroutine 并发对同一备货单发货，断言：成功数正确、∑发货 ≤ 备货量。
// 0.1 M1（锁库存暂缓）：库存不参与校验、不扣减（docs/v0.1/04-architecture §5）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sync"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "PG DSN")
	workers := flag.Int("workers", 8, "并发数")
	qtyPer := flag.Int64("qty", 30, "每次发货量")
	flag.Parse()

	ctx := context.Background()
	r, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer r.Close()

	// 种子：订单 + 备货 100（0.1 发货守恒基准 = 备货量）+ 库存 100（M1 不参与）
	if err := r.SeedDefaultWorld(ctx, "concurrency"); err != nil {
		log.Fatalf("seed world: %v", err)
	}
	key, _ := domain.OrderKey("chang-an", "quick", "concurrency-1")
	ord, created, err := r.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{"run":"concurrency"}`),
		Items: []repo.NewOrderItem{{
			ID: "item-conc", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 500, UnitPriceCents: 1000,
		}},
	})
	if err != nil || !created {
		log.Fatalf("create order: created=%v err=%v", created, err)
	}
	if err := r.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		log.Fatalf("set inventory: %v", err)
	}
	spID := "STK-conc-1"
	tx, err := r.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	if err := r.CreateStockPrepare(ctx, tx, repo.StockPrepare{
		ID: spID, TenantID: "chang-an", OrderID: ord.ID, WarehouseID: "wh-yangyuan",
		State: domain.StateSTKWarehouseConfirm, PlannedQty: 100, RemainingQty: 100,
		BatchSeq: 1, IdempotencyKey: "sp-conc-1",
	}, []repo.StockPrepareItem{{
		ID: "spi-conc", TenantID: "chang-an", StockPrepareID: spID,
		OrderItemID: "item-conc", ProductID: "prod-c68", Qty: 100,
	}}); err != nil {
		log.Fatalf("stock prepare: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}

	// 并发发货
	var wg sync.WaitGroup
	success, failures := 0, 0
	var mu sync.Mutex
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx, err := r.Begin(ctx)
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			defer tx.Rollback(ctx)
			err = r.ApplyShipment(ctx, tx, repo.Shipment{
				ID: fmt.Sprintf("SHP-conc-%d", n), TenantID: "chang-an", OrderID: ord.ID,
				StockPrepareID: spID, WarehouseID: "wh-yangyuan",
				State: domain.ShipmentStateShipped, PlannedQty: 500, ShippedQty: *qtyPer,
				BatchSeq: n + 1, IdempotencyKey: fmt.Sprintf("ship-conc-%d", n),
			}, []repo.ShipmentItem{{
				ID: fmt.Sprintf("si-conc-%d", n), TenantID: "chang-an", ShipmentID: fmt.Sprintf("SHP-conc-%d", n),
				OrderItemID: "item-conc", ProductID: "prod-c68", Qty: *qtyPer,
			}}, "u-wh-yangyuan")
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			if err := tx.Commit(ctx); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			mu.Lock()
			success++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	inv, err := r.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if err != nil {
		log.Fatalf("inventory: %v", err)
	}
	rows, err := r.ReconcileAllOrders(ctx, "chang-an")
	if err != nil {
		log.Fatalf("reconcile: %v", err)
	}
	violations := 0
	for _, rc := range rows {
		if len(rc.Violations) > 0 {
			violations++
		}
	}
	fmt.Printf("[concurrency] success=%d failures=%d inventory=%d violations=%d\n",
		success, failures, inv.Qty, violations)
	// 0.1 M1：库存不变（锁库存暂缓）
	if inv.Qty != 100 {
		log.Fatalf("concurrency FAILED: 0.1 inventory must be untouched (M1), got %d", inv.Qty)
	}
	if violations > 0 {
		log.Fatalf("concurrency FAILED: conservation broken")
	}
	// 备货 100 / 每次 30 → 最多 3 次成功（∑发货 ≤ ∑备货）
	if success > 3 {
		log.Fatalf("concurrency FAILED: overshipped (success=%d > 3)", success)
	}
	fmt.Println("[concurrency] PASS")
}

func strPtr(s string) *string { return &s }
