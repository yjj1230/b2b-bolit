// cmd/sync —— 飞书数据同步 Job（docs/v0.1/04-architecture §6 / 11-migration）
//
// 0.1 新增组件：周期性把飞书侧数据（数据库/多维表/三方审批流）单向只读同步进本系统 PG，
// 映射为订单/事件/追溯视图，商务在本系统流里可见可操作；0.1 不回写飞书。
// 调研清单 T1-T3 完成前按「结构未知以配置驱动」原则运行：源/列映射/游标全部配置化，
// 调研结果回填 config JSON 即可，不阻塞上线窗口。
//
// 运行：
//
//	go run ./cmd/sync -config config/sync.example.json [-dsn ...] [-since ...]   # 单次运行
//	go run ./cmd/sync -config config/sync.example.json -interval 5m -state-file /tmp/sync-last-ok  # 周期运行
//
// 幂等：migration_batches(tenant,source,checksum) 锚点，同批次重跑不重复（V1）。
// -interval>0 时每轮完整跑一遍全部源，成功后写 state-file（供健康探针），随后等待下一周期。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/sync"
)

func main() {
	cfgPath := flag.String("config", "config/sync.example.json", "同步配置 JSON 路径")
	dsn := flag.String("dsn", "", "本系统 PG DSN（默认取 B2B_PG_DSN）")
	since := flag.String("since", "", "增量游标起点（空 = 首次全量；周期模式下仅首轮使用）")
	interval := flag.Duration("interval", 0, "同步周期（如 5m）；0 = 只跑一轮后退出")
	stateFile := flag.String("state-file", "", "每轮成功后的探针文件（记录时间戳）")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if *interval < 0 {
		log.Fatalf("interval must be >= 0")
	}
	if *interval > 0 && *stateFile == "" {
		log.Fatalf("-state-file is required when -interval is set (compose healthcheck)")
	}

	ctx := context.Background()
	if *dsn == "" {
		*dsn = config.Load().PGDSN
	}
	store, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer store.Close()

	cfg, err := sync.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	run := func() {
		if err := runOnce(ctx, store, cfg, *since); err != nil {
			log.Printf("sync run failed: %v", err)
			return
		}
		// 首轮之后按源游标进入增量（since 仅在首轮生效）
		*since = ""
		touchStateFile(*stateFile)
	}

	run()
	if *interval == 0 {
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		run()
	}
}

// touchStateFile 写健康探针文件（内容为完成时间戳）。
func touchStateFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		log.Printf("write state file: %v", err)
	}
}

// runOnce 执行一轮同步：遍历全部源，拉取 → 敏感剥离 → 映射 → 幂等批次落库。
func runOnce(ctx context.Context, store *repo.Repo, cfg *sync.Config, since string) error {
	for _, sc := range cfg.Sources {
		if sc.TenantID == "" {
			sc.TenantID = cfg.TenantID
		}
		fetcher, err := sync.NewFetcher(sc)
		if err != nil {
			log.Printf("source %s: %v", sc.Name, err)
			continue
		}
		batch, err := fetcher.Fetch(ctx, since)
		if err != nil {
			log.Printf("source %s fetch: %v", sc.Name, err)
			continue
		}
		if len(batch.Rows) == 0 {
			log.Printf("source %s: no new rows (since=%s)", sc.Name, since)
			continue
		}
		checksum := sync.BatchChecksum(batch.Rows)
		applied, err := store.BatchApplied(ctx, cfg.TenantID, sc.Name, checksum)
		if err != nil {
			log.Printf("source %s anchor check: %v", sc.Name, err)
			continue
		}
		if applied {
			log.Printf("source %s: batch %s already applied (idempotent skip)", sc.Name, checksum[:16])
			continue
		}
		orders := make([]repo.SyncOrder, 0, len(batch.Rows))
		for _, row := range batch.Rows {
			row = sync.SanitizeRow(row) // V4：敏感字段零残留
			mapped, err := sync.MapRow(sc, row)
			if err != nil {
				log.Printf("source %s map row: %v", sc.Name, err)
				continue
			}
			orders = append(orders, repo.SyncOrder{
				OrderID: mapped.OrderID, CustomerID: mapped.CustomerID, State: mapped.State,
				PaymentTerm: mapped.PaymentTerm, SupplierChannel: mapped.SupplierChannel,
				IdempotencyKey: mapped.IdempotencyKey, PayloadHash: mapped.PayloadHash, Payload: mapped.Payload,
				LegacyFlowID: mapped.LegacyFlowID,
				EventAction:  mapped.EventAction, EventReason: mapped.EventReason,
				EventOperator: mapped.EventOperator, EventTime: mapped.EventTime,
				LegacyStageID: mapped.LegacyStageID, LegacyInstanceID: mapped.LegacyInstanceID,
				LegacyTaskID: mapped.LegacyTaskID, StageCode: mapped.StageCode,
			})
		}
		ok, err := store.ApplySyncBatch(ctx, cfg.TenantID, sc.Name, checksum, orders)
		if err != nil {
			log.Printf("source %s apply batch: %v", sc.Name, err)
			continue
		}
		log.Printf("source %s: applied=%v rows=%d cursor=%s checksum=%s", sc.Name, ok, len(orders), batch.Cursor, checksum[:16])
	}
	return nil
}
