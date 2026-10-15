// scripts/pgapi_backfill —— D-006 旧主数据 ID → pgAPI record_id 一次性回填工具。
//
// 目标（project/decisions/D-006 Q2=A）：
//  1. 读取 legacy 本地表 customers/products 的旧 ID 与名称/SKU；
//  2. 通过 pgAPI 按 customer_name / product_name / spec 精确匹配 record_id；
//  3. dry-run（默认）打印映射与未匹配/歧义清单，任何歧义退出码 2；
//  4. -apply 时在单事务内写 pgapi_entity_mappings，并把流程表中
//     customer_id/product_id 改写为 pgAPI record_id。
//
// 安全边界：不修改 legacy 表；只改流程表引用；映射表幂等；任何未匹配/多候选都中止。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"b2b_pilot_v3/internal/pgapi"
)

type mapping struct {
	TenantID   string
	EntityType string
	LegacyID   string
	Name       string
	RecordID   string
	MatchedBy  string
}

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "process PG DSN")
	base := flag.String("base", "https://api.renheng.com/pgapi", "pgAPI base URL")
	key := flag.String("key", "", "pgAPI X-API-Key（必填）")
	tenant := flag.String("tenant", "chang-an", "tenant_id")
	apply := flag.Bool("apply", false, "写入映射并改写流程表（默认 dry-run）")
	flag.Parse()

	if *key == "" {
		log.Fatal("pgAPI API key is required (-key)")
	}
	ctx := context.Background()
	client, err := pgapi.NewClient(*base, *key, nil)
	if err != nil {
		log.Fatalf("pgapi client: %v", err)
	}
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()

	customers, err := loadLegacyCustomers(ctx, pool, *tenant)
	if err != nil {
		log.Fatalf("load legacy customers: %v", err)
	}
	products, err := loadLegacyProducts(ctx, pool, *tenant)
	if err != nil {
		log.Fatalf("load legacy products: %v", err)
	}

	var mappings []mapping
	var problems []string
	recordOwners := map[string]string{} // record_id -> legacy id，防止多旧 ID 折叠到同一 record

	for _, c := range customers {
		rec, matched, err := resolveCustomer(ctx, client, c.ID, c.Name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("customer %s (%s): %v", c.ID, c.Name, err))
			continue
		}
		if prev, dup := recordOwners["c:"+rec]; dup {
			problems = append(problems, fmt.Sprintf("customer %s and %s both map to %s", c.ID, prev, rec))
			continue
		}
		recordOwners["c:"+rec] = c.ID
		mappings = append(mappings, mapping{TenantID: *tenant, EntityType: "customer", LegacyID: c.ID, Name: c.Name, RecordID: rec, MatchedBy: matched})
	}
	for _, p := range products {
		rec, matched, err := resolveProduct(ctx, client, p.ID, p.SKU, p.Name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("product %s (%s/%s): %v", p.ID, p.SKU, p.Name, err))
			continue
		}
		if prev, dup := recordOwners["p:"+rec]; dup {
			problems = append(problems, fmt.Sprintf("product %s and %s both map to %s", p.ID, prev, rec))
			continue
		}
		recordOwners["p:"+rec] = p.ID
		mappings = append(mappings, mapping{TenantID: *tenant, EntityType: "product", LegacyID: p.ID, Name: p.SKU + " / " + p.Name, RecordID: rec, MatchedBy: matched})
	}

	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].EntityType != mappings[j].EntityType {
			return mappings[i].EntityType < mappings[j].EntityType
		}
		return mappings[i].LegacyID < mappings[j].LegacyID
	})
	for _, m := range mappings {
		fmt.Printf("MATCH %-8s %-24s -> %-24s by %s\n", m.EntityType, m.LegacyID, m.RecordID, m.MatchedBy)
	}
	for _, p := range problems {
		fmt.Printf("FAIL  %s\n", p)
	}
	fmt.Printf("summary: customers=%d products=%d matched=%d problems=%d\n", len(customers), len(products), len(mappings), len(problems))

	if len(problems) > 0 {
		fmt.Println("backfill aborted: unresolved legacy ids; no data was modified")
		os.Exit(2)
	}
	if !*apply {
		fmt.Println("dry-run ok: rerun with -apply to update process tables")
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, m := range mappings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pgapi_entity_mappings (tenant_id, entity_type, legacy_id, record_id, matched_by)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (tenant_id, entity_type, legacy_id) DO UPDATE
			SET record_id=EXCLUDED.record_id, matched_by=EXCLUDED.matched_by, matched_at=now()`,
			m.TenantID, m.EntityType, m.LegacyID, m.RecordID, m.MatchedBy); err != nil {
			log.Fatalf("insert mapping: %v", err)
		}
		if m.EntityType == "customer" {
			if _, err := tx.Exec(ctx, `
				UPDATE orders SET customer_id=$3, updated_at=now()
				WHERE tenant_id=$1 AND customer_id=$2`, m.TenantID, m.LegacyID, m.RecordID); err != nil {
				log.Fatalf("update orders.customer_id: %v", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE customer_contacts SET customer_id=$3, updated_at=now()
				WHERE tenant_id=$1 AND customer_id=$2`, m.TenantID, m.LegacyID, m.RecordID); err != nil {
				log.Fatalf("update customer_contacts.customer_id: %v", err)
			}
		} else {
			for _, stmt := range []string{
				`UPDATE order_items SET product_id=$3 WHERE tenant_id=$1 AND product_id=$2`,
				`UPDATE stock_prepare_items SET product_id=$3 WHERE tenant_id=$1 AND product_id=$2`,
				`UPDATE shipment_items SET product_id=$3 WHERE tenant_id=$1 AND product_id=$2`,
				`UPDATE invoice_items SET product_id=$3 WHERE tenant_id=$1 AND product_id=$2`,
				`UPDATE inventory_items SET product_id=$3 WHERE tenant_id=$1 AND product_id=$2`,
			} {
				if _, err := tx.Exec(ctx, stmt, m.TenantID, m.LegacyID, m.RecordID); err != nil {
					log.Fatalf("update product refs: %v", err)
				}
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("backfill applied: %d mappings committed\n", len(mappings))
}

type legacyCustomer struct{ ID, Name string }
type legacyProduct struct{ ID, SKU, Name string }

func loadLegacyCustomers(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]legacyCustomer, error) {
	rows, err := pool.Query(ctx, `SELECT id, name FROM customers WHERE tenant_id=$1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyCustomer
	for rows.Next() {
		var c legacyCustomer
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadLegacyProducts(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]legacyProduct, error) {
	rows, err := pool.Query(ctx, `SELECT id, sku, name FROM products WHERE tenant_id=$1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyProduct
	for rows.Next() {
		var p legacyProduct
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func resolveCustomer(ctx context.Context, client *pgapi.Client, legacyID, name string) (recordID, matchedBy string, err error) {
	res, err := client.SearchCustomers(ctx, pgapi.Query{PageSize: 200, Filters: map[string]string{"customer_name": name}})
	if err != nil {
		return "", "", err
	}
	var exact []pgapi.Customer
	for _, c := range res.Items {
		if strings.TrimSpace(c.CustomerName) == strings.TrimSpace(name) {
			exact = append(exact, c)
		}
	}
	switch len(exact) {
	case 0:
		return "", "", fmt.Errorf("no exact customer_name match in pgAPI")
	case 1:
		return exact[0].RecordID, "customer_name", nil
	default:
		return "", "", fmt.Errorf("%d exact customer_name matches (ambiguous)", len(exact))
	}
}

func resolveProduct(ctx context.Context, client *pgapi.Client, legacyID, sku, name string) (recordID, matchedBy string, err error) {
	seen := map[string]pgapi.Product{}
	add := func(res *pgapi.ListResult[pgapi.Product], err error) error {
		if err != nil {
			return err
		}
		for _, p := range res.Items {
			seen[p.RecordID] = p
		}
		return nil
	}
	if err := add(client.SearchProducts(ctx, pgapi.Query{PageSize: 200, Filters: map[string]string{"product_name": name}})); err != nil {
		return "", "", err
	}
	if err := add(client.SearchProducts(ctx, pgapi.Query{PageSize: 200, Filters: map[string]string{"spec": sku}})); err != nil {
		return "", "", err
	}
	var exact []pgapi.Product
	for _, p := range seen {
		if strings.TrimSpace(p.ProductName) == strings.TrimSpace(name) || strings.TrimSpace(p.Spec) == strings.TrimSpace(sku) {
			exact = append(exact, p)
		}
	}
	switch len(exact) {
	case 0:
		return "", "", fmt.Errorf("no exact product_name/spec match in pgAPI")
	case 1:
		matchedBy := "product_name"
		if strings.TrimSpace(exact[0].Spec) == strings.TrimSpace(sku) {
			matchedBy = "spec"
		}
		return exact[0].RecordID, matchedBy, nil
	default:
		return "", "", fmt.Errorf("%d exact product matches (ambiguous)", len(exact))
	}
}
