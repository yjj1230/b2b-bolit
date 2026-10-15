//go:build integration

package shadow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// L1 影子仓储：真实 PostgreSQL（PILOT_TEST_PG_DSN；默认 55432/b2b_pilot_test）。
// schema = migrations/*.sql（001-007）+ migrations/shadow/*.sql（008），
// 与 A-02 影子库迁移口径一致（主 migrate 顶层 glob 不递归子目录）。

const shadowDefaultDSN = "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"

type shadowTestEnv struct {
	store *Store
	repo  *repo.Repo
	conn  *pgx.Conn
}

func newShadowTestEnv(t *testing.T) *shadowTestEnv {
	t.Helper()
	dsn := os.Getenv("PILOT_TEST_PG_DSN")
	if dsn == "" {
		dsn = shadowDefaultDSN
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable (%v) — shadow L1 test skipped", err)
	}
	r, err := repo.New(ctx, dsn)
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	shadowFiles, err := filepath.Glob("../../migrations/shadow/*.sql")
	if err != nil {
		t.Fatalf("glob shadow migrations: %v", err)
	}
	for _, f := range append(files, shadowFiles...) {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
	return &shadowTestEnv{store: store, repo: r, conn: conn}
}

func (e *shadowTestEnv) cleanup() {
	e.store.Close()
	e.repo.Close()
	_ = e.conn.Close(context.Background())
}

// S-04 · shadow-ingest：同批次重跑无重复（migration_batches checksum）；
// 源事件 append-only；敏感字段由 sync.SanitizeRow 在 cmd 侧剥离（本测试验证仓储幂等）。
func TestS04SourceEventBatchIdempotent(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	rows := []SourceEventRow{
		{TenantID: "chang-an", LegacyFlowID: "flow-1", LegacyEventID: "e1",
			StageCode: "ORD", Action: "创建订单", Payload: map[string]any{"customer_id": "cust-1"},
			OccurredAt: time.Now(), Source: "orders", Checksum: "c1"},
		{TenantID: "chang-an", LegacyFlowID: "flow-1", LegacyEventID: "e2",
			StageCode: "ORD", Action: "主管通过", Payload: map[string]any{"verdict": "APPROVED"},
			OccurredAt: time.Now(), Source: "orders", Checksum: "c2"},
	}
	n, err := env.store.InsertSourceEvents(ctx, rows)
	if err != nil || n != 2 {
		t.Fatalf("first insert: n=%d err=%v", n, err)
	}
	n, err = env.store.InsertSourceEvents(ctx, rows)
	if err != nil || n != 0 {
		t.Fatalf("replay insert must be no-op: n=%d err=%v", n, err)
	}
	all, err := env.store.ListSourceEvents(ctx, "chang-an", "", 100)
	if err != nil || len(all) != 2 {
		t.Fatalf("source events = %d err=%v (want 2 append-only)", len(all), err)
	}
	// 批次锚点（cmd 用同一 checksum 判重）
	if err := env.store.MarkBatchApplied(ctx, "chang-an", "shadow:orders", "batch-1"); err != nil {
		t.Fatalf("mark batch: %v", err)
	}
	applied, err := env.store.BatchApplied(ctx, "chang-an", "shadow:orders", "batch-1")
	if err != nil || !applied {
		t.Fatalf("batch anchor not recorded: applied=%v err=%v", applied, err)
	}
}

// S-08 · shadow-audit 数据面：覆盖率矩阵 + 分歧去重 + 迁移扫描 + 卡住/对齐查询。
func TestS08CoverageDivergenceAndAuditQueries(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	// 79 边基线
	n, err := env.store.EnsureCoverageBaseline(ctx, "real")
	if err != nil || n != 79 {
		t.Fatalf("baseline edges = %d err=%v (want 79)", n, err)
	}
	// 真实命中 +1
	if err := env.store.HitCoverage(ctx, "ORD", domain.StateORDRequested, domain.StateORDPriceReviewing,
		time.Now(), "real"); err != nil {
		t.Fatalf("hit coverage: %v", err)
	}
	cov, err := env.store.ListCoverage(ctx)
	if err != nil || len(cov) != 79 {
		t.Fatalf("coverage rows = %d err=%v", len(cov), err)
	}
	for _, c := range cov {
		if c.FSM == "ORD" && c.From == domain.StateORDRequested && c.To == domain.StateORDPriceReviewing {
			if c.HitCount != 1 {
				t.Fatalf("hit count = %d, want 1", c.HitCount)
			}
		}
	}

	// 分歧同 run 幂等去重
	d := NewDivergence(KindMapping, "", ReasonUnmappedEvent, "flow-x", "unmapped stage=ORD action=奇怪动作")
	if err := env.store.BeginRun(ctx, "run-1", "ing", "feed", "test"); err != nil {
		t.Fatalf("begin run: %v", err)
	}
	_, created, err := env.store.RecordDivergence(ctx, "run-1", d)
	if err != nil || !created {
		t.Fatalf("first divergence: created=%v err=%v", created, err)
	}
	_, created, err = env.store.RecordDivergence(ctx, "run-1", d)
	if err != nil || created {
		t.Fatalf("duplicate divergence: created=%v err=%v (want no-op)", created, err)
	}
	sum, err := env.store.ListDivergenceSummary(ctx)
	if err != nil || len(sum) != 1 || sum[0].Count != 1 {
		t.Fatalf("divergence summary = %+v err=%v", sum, err)
	}

	// 真实订单事件链 → 迁移扫描（FSM 归属 + 覆盖）
	if err := env.repo.SeedTenant(ctx, "chang-an", "长安"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ord, created, err := env.repo.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到",
		IdempotencyKey: "shadow:flow-y", Payload: []byte(`{}`),
		Items: []repo.NewOrderItem{{ID: "item-y", ProductID: "prod-c68", Quantity: 5, UnitPriceCents: 1000}},
	})
	if err != nil || !created {
		t.Fatalf("create order: created=%v err=%v", created, err)
	}
	if err := env.repo.UpdateOrderStateTx(ctx, "chang-an", ord.ID,
		domain.StateORDRequested, domain.StateORDPriceReviewing, "PriceReviewStarted", "system", "ok", nil); err != nil {
		t.Fatalf("update state: %v", err)
	}
	trs, err := env.store.ListOrderTransitions(ctx, "chang-an", 100)
	if err != nil || len(trs) != 1 {
		t.Fatalf("transitions = %d err=%v", len(trs), err)
	}
	if got := FSMForStates(trs[0].FromState, trs[0].ToState); got != "ORD" {
		t.Fatalf("fsm for transition = %q, want ORD", got)
	}

	// 卡住/对齐查询可执行且不误报（新单 = 未卡住；无源快照 = 不对齐）
	stuck, err := env.store.ListStuckOrders(ctx, "chang-an", 24*time.Hour)
	if err != nil || len(stuck) != 0 {
		t.Fatalf("stuck = %+v err=%v (fresh order must not be stuck)", stuck, err)
	}
	align, err := env.store.ListStateAlignment(ctx, "chang-an", 100)
	if err != nil || len(align) != 0 {
		t.Fatalf("alignment = %+v err=%v", align, err)
	}
}

// Store 全方法 happy path（L1 覆盖补齐：run 档案 / 信号台账 / 流恢复 / 卡住 / 对齐 / 错误分支）。
func TestStoreAllMethodsHappyPaths(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()
	if env.store.Pool() == nil {
		t.Fatal("Pool() returned nil")
	}
	if err := env.store.BeginRun(ctx, "run-all", "ing", "feed", "all"); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if err := env.store.FinishRun(ctx, "run-all", "COMPLETED", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	rows := []SourceEventRow{
		{TenantID: "chang-an-shadow", LegacyFlowID: "flow-store", LegacyEventID: "s1",
			StageCode: "ORD", Action: "创建订单", Payload: map[string]any{"customer_id": "rec-c"},
			Source: "orders", Checksum: "c1"},
		{TenantID: "chang-an-shadow", LegacyFlowID: "flow-store", LegacyEventID: "s2",
			StageCode: "ORD", Action: "主管通过", Payload: map[string]any{"operator": "u"},
			Source: "orders", Checksum: "c2"},
		{TenantID: "chang-an-shadow", LegacyFlowID: "flow-store", LegacyEventID: "s3",
			StageCode: "ORD", Action: "状态快照", Payload: map[string]any{"state": "INV_CLOSED"},
			Source: "orders", Checksum: "c3"},
	}
	if _, err := env.store.InsertSourceEvents(ctx, rows); err != nil {
		t.Fatalf("InsertSourceEvents: %v", err)
	}
	// nil payload → NOT NULL 违反（错误分支）
	if _, err := env.store.InsertSourceEvents(ctx, []SourceEventRow{{
		TenantID: "chang-an-shadow", LegacyFlowID: "flow-bad", LegacyEventID: "b1",
		Action: "x", Payload: nil, Source: "orders", Checksum: "c4",
	}}); err == nil {
		t.Fatal("nil payload insert must fail")
	}
	events, err := env.store.ListSourceEvents(ctx, "chang-an-shadow", "NEW", 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("NEW events = %d err=%v", len(events), err)
	}
	if err := env.store.MarkSourceEventState(ctx, events[0].ID, "FED"); err != nil {
		t.Fatalf("MarkSourceEventState: %v", err)
	}
	fed, err := env.store.ListSourceEvents(ctx, "chang-an-shadow", "FED", 10)
	if err != nil || len(fed) != 1 {
		t.Fatalf("FED events = %d err=%v", len(fed), err)
	}

	// 信号台账：插入幂等 + DELIVERED/FAILED + 失败计数
	logID, created, err := env.store.UpsertSignalLog(ctx, SignalLogEntry{
		SourceEventID: events[0].ID, Signal: "price-review", Payload: map[string]any{"verdict": "APPROVED"}, WorkflowID: "wf-1",
	})
	if err != nil || !created || logID == 0 {
		t.Fatalf("UpsertSignalLog insert: id=%d created=%v err=%v", logID, created, err)
	}
	if _, created, err := env.store.UpsertSignalLog(ctx, SignalLogEntry{
		SourceEventID: events[0].ID, Signal: "price-review", Payload: map[string]any{"verdict": "APPROVED"}, WorkflowID: "wf-1",
	}); err != nil || created {
		t.Fatalf("UpsertSignalLog conflict must be idempotent: created=%v err=%v", created, err)
	}
	if err := env.store.MarkSignalDelivered(ctx, logID); err != nil {
		t.Fatalf("MarkSignalDelivered: %v", err)
	}
	failID, _, err := env.store.UpsertSignalLog(ctx, SignalLogEntry{
		SourceEventID: events[1].ID, Signal: "price-review", Payload: map[string]any{}, WorkflowID: "",
	})
	if err != nil {
		t.Fatalf("UpsertSignalLog fail row: %v", err)
	}
	if err := env.store.MarkSignalFailed(ctx, failID, "boom"); err != nil {
		t.Fatalf("MarkSignalFailed: %v", err)
	}
	if n, err := env.store.SignalFailureCount(ctx); err != nil || n != 1 {
		t.Fatalf("SignalFailureCount = %d err=%v", n, err)
	}

	// 流恢复：影子订单 + workflow_id 台账
	flowID := "flow-store"
	if err := env.repo.SeedTenant(ctx, "chang-an-shadow", "长安·影子"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, _, err := env.repo.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an-shadow", CustomerID: "rec-c", PaymentTerm: "款到",
		IdempotencyKey: "order:chang-an-shadow:quick:shadow-flow-store",
		Payload:        []byte(`{}`), SubmissionMode: "shadow", LegacyFlowID: &flowID,
		Items: []repo.NewOrderItem{{ID: "item-store", ProductID: "rec-p", Quantity: 1, UnitPriceCents: 100}},
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	startedFlows, err := env.store.ListStartedFlowIDs(ctx, "chang-an-shadow")
	if err != nil || len(startedFlows) != 1 || startedFlows[0] != "flow-store" {
		t.Fatalf("started flows = %v err=%v", startedFlows, err)
	}
	if wid, err := env.store.LatestWorkflowIDForFlow(ctx, "chang-an-shadow", "flow-store"); err != nil || wid != "wf-1" {
		t.Fatalf("LatestWorkflowIDForFlow = %q err=%v", wid, err)
	}

	// 卡住清单：把最新事件拨到 30 天前
	if _, err := env.conn.Exec(ctx, `
		UPDATE order_events SET occurred_at = now() - interval '30 days'
		WHERE order_id = (SELECT id FROM orders WHERE legacy_flow_id='flow-store')`); err != nil {
		t.Fatalf("age events: %v", err)
	}
	stuck, err := env.store.ListStuckOrders(ctx, "chang-an-shadow", 24*time.Hour)
	if err != nil || len(stuck) != 1 {
		t.Fatalf("stuck = %+v err=%v", stuck, err)
	}

	// 状态对齐：源快照 INV_CLOSED vs 影子订单 ORD_REQUESTED → 必须入列
	align, err := env.store.ListStateAlignment(ctx, "chang-an-shadow", 100)
	if err != nil || len(align) != 1 || align[0].ShadowState != domain.StateORDRequested || align[0].SourceState != "INV_CLOSED" {
		t.Fatalf("alignment = %+v err=%v", align, err)
	}

	// 覆盖率首命中后 source 由 seed 转 real；真实命中后 seed 命中不得回退 source。
	if _, err := env.store.EnsureCoverageBaseline(ctx, "seed"); err != nil {
		t.Fatalf("EnsureCoverageBaseline: %v", err)
	}
	if err := env.store.HitCoverage(ctx, "ORD", domain.StateORDRequested, domain.StateORDPriceReviewing, time.Now(), "real"); err != nil {
		t.Fatalf("HitCoverage: %v", err)
	}
	if err := env.store.HitCoverage(ctx, "ORD", domain.StateORDRequested, domain.StateORDPriceReviewing, time.Now(), "seed"); err != nil {
		t.Fatalf("HitCoverage seed after real: %v", err)
	}
	cov, err := env.store.ListCoverage(ctx)
	if err != nil || len(cov) != 79 {
		t.Fatalf("ListCoverage = %d err=%v", len(cov), err)
	}
	for _, c := range cov {
		if c.FSM == "ORD" && c.From == domain.StateORDRequested && c.To == domain.StateORDPriceReviewing {
			if c.Source != "real" || c.HitCount != 2 {
				t.Fatalf("edge source=%q hits=%d, want real/2", c.Source, c.HitCount)
			}
		}
	}
}

// 发布后场景 · A-02 迁移隔离：008 只进影子库，五表齐备且影子租户基座存在；
// orders.submission_mode CHECK 已放宽为含 shadow（主 migrate 顶层 glob 不会触达本文件）。
func TestReleaseScenarioMigrationIsolation(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	var n int
	if err := env.conn.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema='public' AND table_name LIKE 'shadow\_%'`).Scan(&n); err != nil {
		t.Fatalf("count shadow tables: %v", err)
	}
	if n != 5 {
		t.Fatalf("shadow tables = %d, want 5", n)
	}
	var constraint string
	if err := env.conn.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname='orders_submission_mode_check'`).Scan(&constraint); err != nil {
		t.Fatalf("submission_mode constraint: %v", err)
	}
	if !strings.Contains(constraint, "'shadow'") {
		t.Fatalf("submission_mode check does not allow shadow: %s", constraint)
	}
	var tenants, users int
	if err := env.conn.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id='chang-an-shadow'`).Scan(&tenants); err != nil {
		t.Fatalf("tenant seed: %v", err)
	}
	if err := env.conn.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id='chang-an-shadow'`).Scan(&users); err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if tenants != 1 || users != 6 {
		t.Fatalf("shadow base seed tenants=%d users=%d, want 1/6", tenants, users)
	}
}

// 发布后场景 · feed 重启接续：FED 事件不再出现在 NEW；workflow_id 可从台账恢复；
// signal log 幂等（同一事件同一信号只一行），失败后重试可转 DELIVERED。
func TestReleaseScenarioRestartRecoveryAndSignalLog(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	rows := []SourceEventRow{
		{TenantID: "chang-an-shadow", LegacyFlowID: "flow-restart", LegacyEventID: "r1",
			StageCode: "ORD", Action: "创建订单", Payload: map[string]any{"customer_id": "c"},
			Source: "orders", Checksum: "c1"},
		{TenantID: "chang-an-shadow", LegacyFlowID: "flow-restart", LegacyEventID: "r2",
			StageCode: "ORD", Action: "主管通过", Payload: map[string]any{"operator": "u"},
			Source: "orders", Checksum: "c2"},
	}
	if _, err := env.store.InsertSourceEvents(ctx, rows); err != nil {
		t.Fatalf("insert events: %v", err)
	}
	events, err := env.store.ListSourceEvents(ctx, "chang-an-shadow", "NEW", 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("NEW = %d err=%v", len(events), err)
	}
	// 第一轮：事件 r1 建单成功（FED），r2 投递成功并记录 workflow_id。
	if err := env.store.MarkSourceEventState(ctx, events[0].ID, "FED"); err != nil {
		t.Fatalf("mark FED: %v", err)
	}
	logID, created, err := env.store.UpsertSignalLog(ctx, SignalLogEntry{
		SourceEventID: events[1].ID, Signal: "price-review",
		Payload: map[string]any{"verdict": "APPROVED"}, WorkflowID: "wf-restart",
	})
	if err != nil || !created {
		t.Fatalf("upsert signal: created=%v err=%v", created, err)
	}
	if err := env.store.MarkSignalDelivered(ctx, logID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if err := env.store.MarkSourceEventState(ctx, events[1].ID, "FED"); err != nil {
		t.Fatalf("mark FED r2: %v", err)
	}

	// 重启后：NEW 为空；workflow_id 可恢复；信号日志幂等。
	newEvents, err := env.store.ListSourceEvents(ctx, "chang-an-shadow", "NEW", 10)
	if err != nil || len(newEvents) != 0 {
		t.Fatalf("NEW after round = %d err=%v", len(newEvents), err)
	}
	wid, err := env.store.LatestWorkflowIDForFlow(ctx, "chang-an-shadow", "flow-restart")
	if err != nil || wid != "wf-restart" {
		t.Fatalf("workflow id recovery = %q err=%v", wid, err)
	}
	if _, created, err := env.store.UpsertSignalLog(ctx, SignalLogEntry{
		SourceEventID: events[1].ID, Signal: "price-review",
		Payload: map[string]any{"verdict": "APPROVED"}, WorkflowID: "wf-restart",
	}); err != nil || created {
		t.Fatalf("signal log must be idempotent: created=%v err=%v", created, err)
	}
	if n, err := env.store.SignalFailureCount(ctx); err != nil || n != 0 {
		t.Fatalf("failure count = %d err=%v", n, err)
	}
}

// 发布后场景 · 审计幂等：同一事件链重跑审计（SetCoverage 重算口径）不重复累加命中。
func TestReleaseScenarioCoverageAuditIdempotent(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	if _, err := env.store.EnsureCoverageBaseline(ctx, "seed"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	first := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	last := first.Add(2 * time.Hour)
	for i := 0; i < 2; i++ {
		if err := env.store.SetCoverage(ctx, "ORD", domain.StateORDRequested, domain.StateORDPriceReviewing,
			3, first, last, "real"); err != nil {
			t.Fatalf("set coverage round %d: %v", i, err)
		}
	}
	cov, err := env.store.ListCoverage(ctx)
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	for _, c := range cov {
		if c.FSM == "ORD" && c.From == domain.StateORDRequested && c.To == domain.StateORDPriceReviewing {
			if c.HitCount != 3 || c.Source != "real" {
				t.Fatalf("coverage after double audit run = %+v, want hits=3 real", c)
			}
			return
		}
	}
	t.Fatal("ORD edge missing from coverage")
}

// 发布后场景 · 分歧生命周期：同一 run 内同分歧去重；新 run 可再记录（修复-重放证据链）。
func TestReleaseScenarioDivergenceLifecycle(t *testing.T) {
	env := newShadowTestEnv(t)
	defer env.cleanup()
	ctx := context.Background()

	d := NewDivergence(KindMapping, "", ReasonUnmappedEvent, "flow-x", "unknown action")
	for _, runID := range []string{"run-1", "run-2"} {
		if err := env.store.BeginRun(ctx, runID, "ing", "feed", "lifecycle"); err != nil {
			t.Fatalf("begin run %s: %v", runID, err)
		}
		_, created, err := env.store.RecordDivergence(ctx, runID, d)
		if err != nil || !created {
			t.Fatalf("run %s first record: created=%v err=%v", runID, created, err)
		}
		_, created, err = env.store.RecordDivergence(ctx, runID, d)
		if err != nil || created {
			t.Fatalf("run %s duplicate must no-op: created=%v err=%v", runID, created, err)
		}
		if err := env.store.FinishRun(ctx, runID, "COMPLETED", ""); err != nil {
			t.Fatalf("finish run %s: %v", runID, err)
		}
	}
	sum, err := env.store.ListDivergenceSummary(ctx)
	if err != nil || len(sum) != 1 || sum[0].Count != 2 {
		t.Fatalf("divergence summary = %+v err=%v (want 2 across two replay runs)", sum, err)
	}
}
