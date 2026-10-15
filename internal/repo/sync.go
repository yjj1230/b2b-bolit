package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ── 飞书同步写入（docs/v0.1/11-migration §4/§5；cmd/sync 调用）──
// 单向只读：0.1 不回写飞书；幂等锚点 migration_batches（UNIQUE(tenant,source,checksum)）。

// SyncOrder 同步批次内的订单写入（internal/sync 映射产物；11 §5 映射要点）。
type SyncOrder struct {
	OrderID         string
	CustomerID      string
	State           string
	PaymentTerm     string
	SupplierChannel string
	IdempotencyKey  string
	PayloadHash     string
	Payload         []byte
	LegacyFlowID    string
	// 事件链搬运（不得伪造审计链：action/occurred_at/task_id 来自源）
	EventAction   string
	EventReason   string
	EventOperator string
	EventTime     string
	// 迁移追溯（legacy_flow_mapping）
	LegacyStageID    string
	LegacyInstanceID string
	LegacyTaskID     string
	StageCode        string
	StageSeq         int
}

// ApplySyncBatch 幂等批次写入（单事务）：
//  1. 批次锚点已存在 → applied=false（V1：重跑不重复）；
//  2. orders 快照 upsert（按 legacy_flow_id 取最后一条有效记录）；
//  3. 源事件搬运进 order_events（append-only；event_id 语义由源 occurred_at 保留）；
//  4. legacy_flow_mapping 追溯行 upsert；
//  5. migration_batches 锚点落库。
func (r *Repo) ApplySyncBatch(ctx context.Context, tenantID, source, checksum string, orders []SyncOrder) (applied bool, err error) {
	var exists int64
	err = r.pool.QueryRow(ctx, `
		SELECT id FROM migration_batches WHERE tenant_id=$1 AND source=$2 AND checksum=$3 LIMIT 1`,
		tenantID, source, checksum).Scan(&exists)
	if err == nil {
		return false, nil // 幂等锚点：本批次已应用
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	tx, err := r.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	for _, o := range orders {
		if o.LegacyFlowID == "" {
			return false, fmt.Errorf("sync order %s missing legacy_flow_id", o.OrderID)
		}
		// 快照 upsert：业务主键 = legacy_flow_id；state/payload 取最后一条有效记录
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO orders (id, tenant_id, customer_id, state, payment_term, supplier_channel,
			                    idempotency_key, payload_hash, payload, legacy_flow_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, legacy_flow_id)
			DO UPDATE SET state=EXCLUDED.state, payload=EXCLUDED.payload,
			              payload_hash=EXCLUDED.payload_hash, updated_at=now()`,
			o.OrderID, tenantID, o.CustomerID, o.State, o.PaymentTerm, o.SupplierChannel,
			o.IdempotencyKey, o.PayloadHash, o.Payload, o.LegacyFlowID); err != nil {
			return false, fmt.Errorf("sync upsert order: %w", err)
		}
		if o.EventAction != "" {
			if _, err := tx.tx.Exec(ctx, `
				INSERT INTO order_events (tenant_id, order_id, stage_code, task_id, action, from_state, to_state, reason, detail, error, operator_id, occurred_at)
				VALUES ($1,$2,$3,$4,$5,NULL,NULL,$6,$7,NULL,$8,
				        COALESCE(NULLIF($9,'')::timestamptz, now()))`,
				tenantID, o.OrderID, o.StageCode, nullStr(o.LegacyTaskID), o.EventAction, o.EventReason,
				MarshalJSONDetail(map[string]any{"source": source, "legacy_flow_id": o.LegacyFlowID}),
				nullStr(o.EventOperator), o.EventTime); err != nil {
				return false, fmt.Errorf("sync order event: %w", err)
			}
		}
		// 迁移追溯（flow_id/stage/instance 串回订单主脊）
		if o.LegacyInstanceID != "" {
			if _, err := tx.tx.Exec(ctx, `
				INSERT INTO legacy_flow_mapping (tenant_id, legacy_flow_id, legacy_stage_id, stage_code, stage_seq,
				                                 legacy_instance_id, legacy_task_id, target_type, target_id, target_order_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'order',$8,$8)
				ON CONFLICT (tenant_id, stage_code, legacy_instance_id)
				DO UPDATE SET legacy_task_id=EXCLUDED.legacy_task_id, target_order_id=EXCLUDED.target_order_id`,
				tenantID, o.LegacyFlowID, nullStr(o.LegacyStageID), o.StageCode, o.StageSeq,
				o.LegacyInstanceID, nullStr(o.LegacyTaskID), o.OrderID); err != nil {
				return false, fmt.Errorf("sync legacy mapping: %w", err)
			}
		}
	}
	// 批次锚点（幂等红线）
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO migration_batches (tenant_id, source, checksum, state, finished_at)
		VALUES ($1,$2,$3,'DONE',now())`, tenantID, source, checksum); err != nil {
		return false, fmt.Errorf("sync batch anchor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// BatchApplied 批次锚点查询（V1 验收：同一批次重跑无重复数据）。
func (r *Repo) BatchApplied(ctx context.Context, tenantID, source, checksum string) (bool, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM migration_batches WHERE tenant_id=$1 AND source=$2 AND checksum=$3 LIMIT 1`,
		tenantID, source, checksum).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// nullStr 空串 → NULL。
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
