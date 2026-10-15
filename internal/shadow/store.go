package shadow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"b2b_pilot_v3/internal/domain"
)

// Store 影子区 PostgreSQL 访问层（b2b_shadow 库；008 五表 + 复用 001 的 migration_batches）。
// 与生产 repo 完全分离：本类型只被 shadow-* 命令构造，DSN 先经 shadowguard 校验。
type Store struct {
	pool *pgxpool.Pool
}

// Open 建立影子库连接池。
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("shadow pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("shadow pg ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池。
func (s *Store) Close() { s.pool.Close() }

// Pool 底层连接池（迁移/审计直查场景）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ── shadow_runs ──

// BeginRun 开启运行档案（ingest/feed 配置摘要绑定，重放可追溯）。
func (s *Store) BeginRun(ctx context.Context, runID, ingestDigest, feedDigest, note string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO shadow_runs (run_id, ingest_config_digest, feed_config_digest, note)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (run_id) DO UPDATE SET started_at = now(), status='RUNNING', note=$4`,
		runID, ingestDigest, feedDigest, note)
	return err
}

// FinishRun 结束运行档案。
func (s *Store) FinishRun(ctx context.Context, runID, status, note string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE shadow_runs SET finished_at=now(), status=$2, note=$3 WHERE run_id=$1`,
		runID, status, note)
	return err
}

// ── 批次幂等（复用 001 migration_batches）──

// BatchApplied 批次是否已落库（checksum 锚点；同批次重跑不重复）。
func (s *Store) BatchApplied(ctx context.Context, tenantID, source, checksum string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM migration_batches WHERE tenant_id=$1 AND source=$2 AND checksum=$3)`,
		tenantID, source, checksum).Scan(&exists)
	return exists, err
}

// MarkBatchApplied 记录批次锚点（幂等）。
func (s *Store) MarkBatchApplied(ctx context.Context, tenantID, source, checksum string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO migration_batches (tenant_id, source, checksum, state, finished_at)
		VALUES ($1,$2,$3,'applied',now())
		ON CONFLICT (tenant_id, source, checksum) DO NOTHING`, tenantID, source, checksum)
	return err
}

// ── shadow_source_events ──

// SourceEventRow 源事件落库行。
type SourceEventRow struct {
	TenantID      string
	LegacyFlowID  string
	LegacyEventID string
	StageCode     string
	Action        string
	Payload       map[string]any
	OccurredAt    time.Time
	Source        string
	Checksum      string
}

// InsertSourceEvents 追加源事件（append-only；(tenant,flow,event) 唯一，重复静默跳过）。
// 返回实际新插入行数。
func (s *Store) InsertSourceEvents(ctx context.Context, rows []SourceEventRow) (int, error) {
	inserted := 0
	for _, r := range rows {
		ct, err := s.pool.Exec(ctx, `
			INSERT INTO shadow_source_events
			  (tenant_id, legacy_flow_id, legacy_event_id, stage_code, action, payload, occurred_at, source, checksum)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, legacy_flow_id, legacy_event_id) DO NOTHING`,
			r.TenantID, r.LegacyFlowID, r.LegacyEventID, r.StageCode, r.Action,
			r.Payload, nullTime(r.OccurredAt), r.Source, r.Checksum)
		if err != nil {
			return inserted, fmt.Errorf("insert source event %s/%s: %w", r.LegacyFlowID, r.LegacyEventID, err)
		}
		inserted += int(ct.RowsAffected())
	}
	return inserted, nil
}

// ListSourceEvents 取待投递源事件（NEW 优先；稳定序 (flow, id)）。
func (s *Store) ListSourceEvents(ctx context.Context, tenantID, state string, limit int) ([]SourceEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, legacy_flow_id, legacy_event_id, stage_code, action, payload, occurred_at, source, checksum
		FROM shadow_source_events
		WHERE tenant_id=$1 AND ($2='' OR state=$2)
		ORDER BY legacy_flow_id, id
		LIMIT $3`, tenantID, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceEvent
	for rows.Next() {
		var ev SourceEvent
		var occurred *time.Time
		if err := rows.Scan(&ev.ID, &ev.TenantID, &ev.LegacyFlowID, &ev.LegacyEventID,
			&ev.StageCode, &ev.Action, &ev.Payload, &occurred, &ev.Source, &ev.Checksum); err != nil {
			return nil, err
		}
		if occurred != nil {
			ev.OccurredAt = *occurred
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MarkSourceEventState 事件投递后置状态（NEW → FED / SKIPPED）。
func (s *Store) MarkSourceEventState(ctx context.Context, eventID int64, state string) error {
	if state == "" {
		state = "FED"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE shadow_source_events SET state=$2 WHERE id=$1 AND state='NEW'`, eventID, state)
	return err
}

// ── shadow_signal_log ──

// SignalLogEntry 投递台账行。
type SignalLogEntry struct {
	SourceEventID int64
	Signal        string
	Payload       map[string]any
	WorkflowID    string
}

// UpsertSignalLog 幂等落台账（(source_event_id, signal) 唯一）。
func (s *Store) UpsertSignalLog(ctx context.Context, e SignalLogEntry) (int64, bool, error) {
	var id int64
	created := false
	err := s.pool.QueryRow(ctx, `
		INSERT INTO shadow_signal_log (source_event_id, signal, payload, workflow_id)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (source_event_id, signal) DO UPDATE SET workflow_id = EXCLUDED.workflow_id
		RETURNING id, (xmax = 0)`,
		e.SourceEventID, e.Signal, e.Payload, e.WorkflowID).Scan(&id, &created)
	return id, created, err
}

// MarkSignalDelivered 投递成功。
func (s *Store) MarkSignalDelivered(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE shadow_signal_log SET state='DELIVERED', delivered_at=now(), error='' WHERE id=$1`, id)
	return err
}

// MarkSignalFailed 投递失败留痕。
func (s *Store) MarkSignalFailed(ctx context.Context, id int64, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE shadow_signal_log SET state='FAILED', error=$2 WHERE id=$1`, id, errMsg)
	return err
}

// SignalFailureCount 投递失败数（健康探针口径：FAILED > 0 告警）。
func (s *Store) SignalFailureCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM shadow_signal_log WHERE state='FAILED'`).Scan(&n)
	return n, err
}

// ── shadow_divergences ──

// RecordDivergence 分歧落库（同 run 同流同类同原因同 detail 幂等去重）。
func (s *Store) RecordDivergence(ctx context.Context, runID string, d Divergence) (int64, bool, error) {
	var id int64
	created := false
	err := s.pool.QueryRow(ctx, `
		INSERT INTO shadow_divergences (run_id, order_id, legacy_flow_id, kind, severity, reason, from_state, to_state, detail, status, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (run_id, legacy_flow_id, kind, reason, md5(detail)) DO NOTHING
		RETURNING id, true`,
		runID, d.OrderID, d.LegacyFlowID, d.Kind, d.Severity, d.Reason, d.FromState, d.ToState,
		d.Detail, d.Status, d.Owner).Scan(&id, &created)
	if err == pgx.ErrNoRows {
		return 0, false, nil // 幂等：已存在
	}
	return id, created, err
}

// DivergenceSummary 分歧汇总（shadow-audit 分歧清单）。
type DivergenceSummary struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Count    int    `json:"count"`
}

// ListDivergenceSummary 按 kind/severity/status 聚合。
func (s *Store) ListDivergenceSummary(ctx context.Context) ([]DivergenceSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, severity, status, COUNT(*)
		FROM shadow_divergences GROUP BY kind, severity, status ORDER BY kind, severity, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DivergenceSummary
	for rows.Next() {
		var d DivergenceSummary
		if err := rows.Scan(&d.Kind, &d.Severity, &d.Status, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ── shadow_coverage ──

// EnsureCoverageBaseline 79 边基线（hit_count=0；ON CONFLICT 不覆盖真实命中）。
func (s *Store) EnsureCoverageBaseline(ctx context.Context, source string) (int, error) {
	n := 0
	for _, e := range domain.AllEdges() {
		ct, err := s.pool.Exec(ctx, `
			INSERT INTO shadow_coverage (fsm_name, from_state, to_state, hit_count, source)
			VALUES ($1,$2,$3,0,$4)
			ON CONFLICT (fsm_name, from_state, to_state) DO NOTHING`,
			e.FSM, e.From, e.To, source)
		if err != nil {
			return n, err
		}
		n += int(ct.RowsAffected())
	}
	return n, nil
}

// HitCoverage 命中一条边（同边重复 → 计数 +1 + 更新时间；真实命中优先于 seed 标记）。
func (s *Store) HitCoverage(ctx context.Context, fsm, from, to string, occurred time.Time, source string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO shadow_coverage (fsm_name, from_state, to_state, hit_count, first_hit_at, last_hit_at, source)
		VALUES ($1,$2,$3,1,$4,$4,$5)
		ON CONFLICT (fsm_name, from_state, to_state) DO UPDATE SET
		  hit_count = shadow_coverage.hit_count + 1,
		  last_hit_at = $4,
		  source = CASE
		           WHEN EXCLUDED.source = 'real' AND shadow_coverage.source = 'seed' THEN 'real'
		           WHEN shadow_coverage.hit_count = 0 THEN EXCLUDED.source
		           ELSE shadow_coverage.source END`,
		fsm, from, to, nullTime(occurred), source)
	return err
}

// SetCoverage 覆盖一条边的累计命中（audit 重算口径：同输入重跑结果一致，不重复累加）。
func (s *Store) SetCoverage(ctx context.Context, fsm, from, to string, hits int64, first, last time.Time, source string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO shadow_coverage (fsm_name, from_state, to_state, hit_count, first_hit_at, last_hit_at, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (fsm_name, from_state, to_state) DO UPDATE SET
		  hit_count = EXCLUDED.hit_count,
		  first_hit_at = EXCLUDED.first_hit_at,
		  last_hit_at = EXCLUDED.last_hit_at,
		  source = CASE WHEN EXCLUDED.source = 'real' AND shadow_coverage.source = 'seed' THEN 'real'
		                ELSE EXCLUDED.source END`,
		fsm, from, to, hits, nullTime(first), nullTime(last), source)
	return err
}

// CoverageRow 覆盖率行。
type CoverageRow struct {
	FSM        string    `json:"fsm"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	HitCount   int64     `json:"hit_count"`
	FirstHitAt time.Time `json:"first_hit_at,omitempty"`
	LastHitAt  time.Time `json:"last_hit_at,omitempty"`
	Source     string    `json:"source"`
}

// ListCoverage 全部边命中矩阵。
func (s *Store) ListCoverage(ctx context.Context) ([]CoverageRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT fsm_name, from_state, to_state, hit_count, first_hit_at, last_hit_at, source
		FROM shadow_coverage ORDER BY fsm_name, from_state, to_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoverageRow
	for rows.Next() {
		var r CoverageRow
		var first, last *time.Time
		if err := rows.Scan(&r.FSM, &r.From, &r.To, &r.HitCount, &first, &last, &r.Source); err != nil {
			return nil, err
		}
		if first != nil {
			r.FirstHitAt = *first
		}
		if last != nil {
			r.LastHitAt = *last
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── 审计查询（跨 001-007 业务表 + 008 影子表，只读）──

// OrderTransition 订单事件链中的状态迁移。
type OrderTransition struct {
	OrderID    string
	FromState  string
	ToState    string
	Action     string
	OccurredAt time.Time
}

// ListOrderTransitions 订单状态迁移（from/to 双非空；FSM 由 from 阶段派生）。
func (s *Store) ListOrderTransitions(ctx context.Context, tenantID string, limit int) ([]OrderTransition, error) {
	if limit <= 0 {
		limit = 20000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT order_id, action, from_state, to_state, occurred_at
		FROM order_events
		WHERE tenant_id=$1 AND from_state IS NOT NULL AND from_state<>'' AND to_state IS NOT NULL AND to_state<>''
		ORDER BY id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderTransition
	for rows.Next() {
		var t OrderTransition
		if err := rows.Scan(&t.OrderID, &t.Action, &t.FromState, &t.ToState, &t.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FSMForStates 迁移归属 FSM：from 前缀优先（跨段边由出发段状态机编码，如 ORD→SHP）。
func FSMForStates(from, to string) string {
	if s := domain.Stage(from); s != "" {
		return s
	}
	return domain.Stage(to)
}

// StuckOrder 卡住等待信号超过 N 天的影子工作流（12 §8.2 卡住清单）。
type StuckOrder struct {
	OrderID      string
	State        string
	LastEventAge string
	LastAction   string
}

// ListStuckOrders 非终态且最新事件早于 threshold 的订单。
func (s *Store) ListStuckOrders(ctx context.Context, tenantID string, threshold time.Duration) ([]StuckOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.state,
		       (now() - COALESCE(MAX(e.occurred_at), o.created_at))::text,
		       COALESCE(MAX(e.action), 'OrderRequested')
		FROM orders o
		LEFT JOIN order_events e ON e.tenant_id = o.tenant_id AND e.order_id = o.id
		WHERE o.tenant_id=$1 AND o.state NOT IN ('ORD_REJECTED','INV_CLOSED')
		GROUP BY o.id, o.state, o.created_at
		HAVING now() - COALESCE(MAX(e.occurred_at), o.created_at) > $2::interval
		ORDER BY o.id`, tenantID, threshold.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckOrder
	for rows.Next() {
		var st StuckOrder
		if err := rows.Scan(&st.OrderID, &st.State, &st.LastEventAge, &st.LastAction); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StateAlignmentRow 影子订单终态 vs 源事件快照状态（12 §8.2 状态对齐）。
type StateAlignmentRow struct {
	OrderID     string
	FlowID      string
	ShadowState string
	SourceState string
	LastAction  string
}

// ListStateAlignment 比较 orders.state 与同流最新源事件 payload->>'state'。
// 源事件未携带 state 时以最后一个事件 action 对应的影子终态口径对齐：只列出显式快照状态不等的行。
func (s *Store) ListStateAlignment(ctx context.Context, tenantID string, limit int) ([]StateAlignmentRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, COALESCE(o.legacy_flow_id,''),
		       o.state,
		       COALESCE((SELECT jsonb_extract_path_text(se.payload,'state') FROM shadow_source_events se
		                 WHERE se.tenant_id=o.tenant_id AND se.legacy_flow_id=o.legacy_flow_id
		                 ORDER BY se.id DESC LIMIT 1), ''),
		       COALESCE((SELECT se.action FROM shadow_source_events se
		                 WHERE se.tenant_id=o.tenant_id AND se.legacy_flow_id=o.legacy_flow_id
		                 ORDER BY se.id DESC LIMIT 1), '')
		FROM orders o
		WHERE o.tenant_id=$1 AND o.legacy_flow_id IS NOT NULL
		ORDER BY o.id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateAlignmentRow
	for rows.Next() {
		var r StateAlignmentRow
		if err := rows.Scan(&r.OrderID, &r.FlowID, &r.ShadowState, &r.SourceState, &r.LastAction); err != nil {
			return nil, err
		}
		// 无快照状态 = 无对齐基准，不误报；仅显式状态不等才列入（容忍一个同步周期滞后由调用方判定）
		if r.SourceState != "" && r.ShadowState != r.SourceState {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ListStartedFlowIDs 已有影子订单的流（增量轮次：信号直接接续，不误判链断裂）。
func (s *Store) ListStartedFlowIDs(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT legacy_flow_id FROM orders
		WHERE tenant_id=$1 AND legacy_flow_id IS NOT NULL ORDER BY legacy_flow_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// LatestWorkflowIDForFlow 流最近一次投递的 workflow_id（feed 重启后接续投递用）。
func (s *Store) LatestWorkflowIDForFlow(ctx context.Context, tenantID, flowID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT l.workflow_id FROM shadow_signal_log l
		JOIN shadow_source_events e ON e.id = l.source_event_id
		WHERE e.tenant_id=$1 AND e.legacy_flow_id=$2 AND l.workflow_id<>''
		ORDER BY l.id DESC LIMIT 1`, tenantID, flowID).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return id, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
