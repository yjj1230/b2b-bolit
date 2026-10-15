// cmd/shadow-ingest —— 影子轨只读摄入（docs/v0.1/12-shadow-full-flow.html §5 步骤 1）
//
// 配置驱动拉取 orders + stock_prepare_events + shipment_events + invoice_events + 审批任务结果
// （结构与 cmd/sync 同源：config/shadow-ingest.example.json）→ SanitizeRow 敏感剥离 →
// 批次 checksum 幂等 → shadow_source_events 只追加（S-04）。
//
// 运行：
//
//	go run ./cmd/shadow-ingest -config config/shadow-ingest.example.json -interval 5m -state-file /tmp/shadow-ingest-last-ok
//
// 零回写：只读账号 + 本命令没有任何旧系统写接口；DSN/namespace/queue/channel 经 shadowguard 守卫。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/shadow"
	"b2b_pilot_v3/internal/shadowguard"
	"b2b_pilot_v3/internal/sync"
)

func main() {
	cfgPath := flag.String("config", "", "影子摄入源配置 JSON（默认取 SHADOW_INGEST_CONFIG）")
	dsn := flag.String("dsn", "", "影子库 DSN（默认取 B2B_PG_DSN）")
	interval := flag.Duration("interval", 0, "摄入周期（如 5m）；0 = 只跑一轮后退出")
	stateFile := flag.String("state-file", "", "每轮成功后的探针文件")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := config.Load()
	if *cfgPath == "" {
		*cfgPath = cfg.ShadowIngestConfig
	}
	if *dsn == "" {
		*dsn = cfg.PGDSN
	}
	// SH-13/A-01：影子二进制六项启动守卫（DSN 库名/namespace/queue/WeCom/bridge/SpiceDB）。
	cfg.PGDSN = *dsn
	if err := shadowguard.Validate(cfg); err != nil {
		log.Fatalf("shadow guard: %v", err)
	}
	if *interval < 0 {
		log.Fatalf("interval must be >= 0")
	}
	if *interval > 0 && *stateFile == "" {
		log.Fatalf("-state-file is required when -interval is set (compose healthcheck)")
	}

	ctx := context.Background()
	store, err := shadow.Open(ctx, *dsn)
	if err != nil {
		log.Fatalf("open shadow pg: %v", err)
	}
	defer store.Close()

	sc, err := sync.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load ingest config: %v", err)
	}
	ingestDigest := fileDigest(*cfgPath)

	run := func() {
		runID := fmt.Sprintf("ingest-%s", time.Now().UTC().Format("20060102T150405.000"))
		if err := store.BeginRun(ctx, runID, ingestDigest, "", "shadow-ingest"); err != nil {
			log.Printf("begin run: %v", err)
			return
		}
		err := runOnce(ctx, store, sc)
		if err != nil {
			_ = store.FinishRun(ctx, runID, "FAILED", err.Error())
			log.Printf("shadow ingest run %s failed: %v", runID, err)
			return
		}
		if err := store.FinishRun(ctx, runID, "COMPLETED", ""); err != nil {
			log.Printf("finish run: %v", err)
		}
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

// runOnce 一轮摄入：先拉取全部源，再按源配置关联快照（join_source），
// 随后 敏感剥离 → 幂等批次落库。
func runOnce(ctx context.Context, store *shadow.Store, cfg *sync.Config) error {
	batches := map[string][]sync.Row{}
	for _, sc := range cfg.Sources {
		fetcher, err := sync.NewFetcher(sc)
		if err != nil {
			log.Printf("source %s: %v", sc.Name, err)
			continue
		}
		batch, err := fetcher.Fetch(ctx, "")
		if err != nil {
			log.Printf("source %s fetch: %v", sc.Name, err)
			continue
		}
		batches[sc.Name] = batch.Rows
	}
	for _, sc := range cfg.Sources {
		if sc.TenantID == "" {
			sc.TenantID = cfg.TenantID
		}
		batchRows, ok := batches[sc.Name]
		if !ok || len(batchRows) == 0 {
			log.Printf("source %s: no rows", sc.Name)
			continue
		}
		// 旧系统真实 JSONL：决策事件没有顶层 flow_id，按 instance_id 关联 orders.jsonl 快照补全。
		joinIndex := buildJoinIndex(batches[sc.JoinSource], sc.JoinKeyField)
		checksum := sync.BatchChecksum(batchRows)
		source := "shadow:" + sc.Name
		applied, err := store.BatchApplied(ctx, cfg.TenantID, source, checksum)
		if err != nil {
			log.Printf("source %s anchor check: %v", sc.Name, err)
			continue
		}
		if applied {
			log.Printf("source %s: batch %s already applied (idempotent skip)", sc.Name, checksum[:16])
			continue
		}
		rows := make([]shadow.SourceEventRow, 0, len(batchRows))
		for i, row := range batchRows {
			row = sync.SanitizeRow(row) // 敏感字段零残留（11-migration §6）
			row = enrichRow(row, joinIndex, sc)
			ev, err := sourceEvent(cfg.TenantID, sc, row, i)
			if err != nil {
				log.Printf("source %s row %d: %v (skipped, will re-appear next round)", sc.Name, i, err)
				continue
			}
			rows = append(rows, ev)
		}
		if len(rows) == 0 {
			log.Printf("source %s: no mappable rows", sc.Name)
			continue
		}
		n, err := store.InsertSourceEvents(ctx, rows)
		if err != nil {
			log.Printf("source %s insert: %v", sc.Name, err)
			continue
		}
		if err := store.MarkBatchApplied(ctx, cfg.TenantID, source, checksum); err != nil {
			log.Printf("source %s anchor write: %v", sc.Name, err)
			continue
		}
		log.Printf("source %s: inserted=%d total=%d checksum=%s", sc.Name, n, len(rows), checksum[:16])
	}
	return nil
}

// buildJoinIndex 快照源行 → 以 JoinKeyField 为键的索引（同键取最后一条最新快照）。
func buildJoinIndex(rows []sync.Row, keyField string) map[string]sync.Row {
	if keyField == "" {
		return nil
	}
	index := map[string]sync.Row{}
	for _, r := range rows {
		if k := r[keyField]; k != "" {
			index[k] = r
		}
	}
	return index
}

// enrichRow 按 JoinFields 用快照行补全缺失字段（仅填空，不覆盖已有值）。
func enrichRow(row sync.Row, joinIndex map[string]sync.Row, sc sync.SourceConfig) sync.Row {
	if joinIndex == nil || sc.JoinKeyField == "" {
		return row
	}
	snapshot, ok := joinIndex[row[sc.JoinKeyField]]
	if !ok {
		return row
	}
	for srcCol, snapshotCol := range sc.JoinFields {
		if row[srcCol] == "" {
			row[srcCol] = snapshot[snapshotCol]
		}
	}
	return row
}

// sourceEvent 源行 → 影子源事件（action/stage 来自列映射；event_id 缺省按行 checksum 确定性派生）。
func sourceEvent(tenantID string, sc sync.SourceConfig, row sync.Row, index int) (shadow.SourceEventRow, error) {
	col := func(target string) string { return row[sc.ColumnMap[target]] }
	flowID := col("legacy_flow_id")
	if flowID == "" {
		return shadow.SourceEventRow{}, fmt.Errorf("missing legacy_flow_id")
	}
	eventID := col("legacy_event_id")
	if eventID == "" {
		// 源未给事件 ID：用批内行指纹派生（同批重跑幂等；跨批同流同动作会去重，需源提供真 event_id 才能完全区分）
		eventID = fmt.Sprintf("%s:%d", sync.ShortID(fmt.Sprintf("%s:%s:%s:%d", sc.Name, flowID, sync.BatchChecksum([]sync.Row{row}), index)), index)
	}
	action := col("event_action")
	if action == "" {
		return shadow.SourceEventRow{}, fmt.Errorf("missing event_action for %s/%s", flowID, eventID)
	}
	occurredAt := time.Time{}
	if raw := col("occurred_at"); raw != "" {
		if t, err := sync.ParseEventTime(raw); err == nil {
			occurredAt = t
		}
	}
	payload, _ := json.Marshal(row)
	var pmap map[string]any
	_ = json.Unmarshal(payload, &pmap)
	if pmap == nil {
		pmap = map[string]any{}
	}
	stage := col("stage_code")
	if stage == "" {
		stage = "ORD"
	}
	return shadow.SourceEventRow{
		TenantID: tenantID, LegacyFlowID: flowID, LegacyEventID: eventID,
		StageCode: stage, Action: action, Payload: pmap,
		OccurredAt: occurredAt, Source: sc.Name,
		Checksum: sync.ShortID(string(payload)),
	}, nil
}

// fileDigest 配置摘要（运行档案可追溯：换配置 = 新 run 口径）。
func fileDigest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func touchStateFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		log.Printf("write state file: %v", err)
	}
}
