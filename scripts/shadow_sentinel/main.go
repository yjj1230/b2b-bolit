// scripts/shadow_sentinel —— 生产库写哨兵（docs/v0.1/12 §附录 A · A-06）
//
// 每日校验生产业务库零影子污染：
//  1. 无 shadow_* 表（pg_tables）；
//  2. orders 无影子租户行（tenant_id 含 shadow）且无 submission_mode='shadow'；
//  3. order_events 无 ShadowMode 动作；
//  4. migration_batches 无 shadow: 前缀批次。
//
// 任一命中 → 非零退出 + JSON 违规清单（监控/告警拉停口径）；全绿 → 0 退出 + ok 清单。
//
// 运行：go run ./scripts/shadow_sentinel -dsn "postgres://.../b2b_pilot"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

type finding struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

type report struct {
	OK       bool      `json:"ok"`
	Findings []finding `json:"findings,omitempty"`
}

func main() {
	dsn := flag.String("dsn", "", "生产业务库 DSN（必填；空 = 拒绝执行，防误扫）")
	flag.Parse()
	if *dsn == "" {
		log.Fatalf("-dsn is required (refusing to run sentinel without explicit production DSN)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	var findings []finding

	// 1. shadow_* 表零存在
	var tables []string
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'shadow\_%'`)
	if err != nil {
		log.Fatalf("scan tables: %v", err)
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			log.Fatalf("scan table row: %v", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if rows.Err() != nil {
		log.Fatalf("scan tables: %v", rows.Err())
	}
	for _, t := range tables {
		findings = append(findings, finding{"shadow_tables", "production pg_tables contains " + t})
	}

	// 2. 影子租户行 / 影子建单标识零存在
	// 3. 事件链无 ShadowMode 动作
	// 4. 同步批次无影子源前缀
	// 查询失败本身也算违规（哨兵绝不能假绿），写入 findings 并最终非零退出。
	type countCheck struct {
		name  string
		query string
		desc  string
	}
	for _, c := range []countCheck{
		{"shadow_orders", `
			SELECT COUNT(*) FROM orders
			WHERE tenant_id ILIKE '%shadow%' OR submission_mode = 'shadow'`,
			"orders contains %d shadow rows"},
		{"shadow_events", `
			SELECT COUNT(*) FROM order_events
			WHERE action LIKE '%ShadowMode%' OR action LIKE '%shadow%'`,
			"order_events contains %d shadow actions"},
		{"shadow_batches", `
			SELECT COUNT(*) FROM migration_batches WHERE source LIKE 'shadow:%'`,
			"migration_batches contains %d shadow sources"},
	} {
		var n int64
		if err := pool.QueryRow(ctx, c.query).Scan(&n); err != nil {
			findings = append(findings, finding{c.name, fmt.Sprintf("sentinel query failed: %v", err)})
			continue
		}
		if n > 0 {
			findings = append(findings, finding{c.name, fmt.Sprintf(c.desc, n)})
		}
	}

	rep := report{OK: len(findings) == 0, Findings: findings}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if !rep.OK {
		os.Exit(2)
	}
}
