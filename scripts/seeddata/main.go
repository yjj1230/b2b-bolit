// scripts/seeddata —— 种子数据（L3-06：runID 注入，重复跑幂等）
// 播种演示世界 + 若干订单（覆盖 ORD/STK/SHP/INV 各态），供 reconcile/audit/concurrency 验证。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "PG DSN")
	runID := flag.String("run", "seed-1", "run ID（重复跑幂等锚点）")
	flag.Parse()

	ctx := context.Background()
	r, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer r.Close()

	if err := r.SeedDefaultWorld(ctx, *runID); err != nil {
		log.Fatalf("seed world: %v", err)
	}
	fmt.Printf("[seeddata] world seeded (run=%s)\n", *runID)

	// 订单 A：款到全链（终态 CLOSED）
	keyA, _ := domain.OrderKey("chang-an", "quick", "seed-a-"+*runID)
	ordA, created, err := r.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: keyA, Payload: []byte(fmt.Sprintf(`{"run":%q}`, *runID)),
		Items: []repo.NewOrderItem{{
			ID: "item-seed-a-" + shortID(*runID), ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 100, UnitPriceCents: 1000,
		}},
	})
	if err != nil {
		log.Fatalf("create order A: %v", err)
	}
	if created {
		fmt.Printf("[seeddata] order %s (state=%s)\n", ordA.ID, ordA.State)
	}

	// 订单 B：备货中（款到，已到仓管确认）
	keyB, _ := domain.OrderKey("chang-an", "quick", "seed-b-"+*runID)
	ordB, created, err := r.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0002", PaymentTerm: "款到", SupplierChannel: "渠道B",
		IdempotencyKey: keyB, Payload: []byte(fmt.Sprintf(`{"run":%q}`, *runID)),
		Items: []repo.NewOrderItem{{
			ID: "item-seed-b-" + shortID(*runID), ProductID: "prod-d90", WarehouseID: strPtr("wh-dongguan"),
			Quantity: 50, UnitPriceCents: 2000,
		}},
	})
	if err != nil {
		log.Fatalf("create order B: %v", err)
	}
	if created {
		fmt.Printf("[seeddata] order %s (state=%s)\n", ordB.ID, ordB.State)
	}
	fmt.Println("[seeddata] done")
}

func strPtr(s string) *string { return &s }

func shortID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}
