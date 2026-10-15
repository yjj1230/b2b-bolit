// scripts/reconcile —— 全库对账（L3-02：四守恒聚合）
// ∑备货 ≤ 订单、∑发货 ≤ 备货、∑开票 ≤ 发货（数量维度）、库存恒 ≥ 0。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"b2b_pilot_v3/internal/repo"
)

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "PG DSN")
	tenant := flag.String("tenant", "", "租户过滤（空=全部）")
	flag.Parse()

	ctx := context.Background()
	r, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer r.Close()

	rows, err := r.ReconcileAllOrders(ctx, *tenant)
	if err != nil {
		log.Fatalf("reconcile: %v", err)
	}
	violations := 0
	for _, rc := range rows {
		status := "OK"
		if len(rc.Violations) > 0 {
			status = fmt.Sprintf("VIOLATION %v", rc.Violations)
			violations++
		}
		fmt.Printf("[reconcile] %s/%s order=%d prepared=%d shipped=%d invoiced=%d/%d %s\n",
			rc.TenantID, rc.OrderID, rc.OrderQty, rc.PreparedQty, rc.ShippedQty,
			rc.InvoicedQty, rc.InvoicedAmount, status)
	}
	neg, err := r.ReconcileInventory(ctx, *tenant)
	if err != nil {
		log.Fatalf("reconcile inventory: %v", err)
	}
	for _, it := range neg {
		fmt.Printf("[reconcile] NEGATIVE INVENTORY %s/%s/%s qty=%d\n", it.TenantID, it.WarehouseID, it.ProductID, it.Qty)
		violations++
	}
	fmt.Printf("[reconcile] orders=%d violations=%d\n", len(rows), violations)
	if violations > 0 {
		log.Fatalf("reconcile FAILED: %d violation(s)", violations)
	}
	fmt.Println("[reconcile] PASS")
}
