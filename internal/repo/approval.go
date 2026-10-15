package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── 审批任务（CQ-05 审批链状态 / CQ-10 回单；待办检索 L1-11）──

// NewApprovalTask 审批任务创建请求。
type NewApprovalTask struct {
	TenantID     string
	OrderID      string
	StageCode    string // ORD/STK/SHP/INV
	Kind         string // PRICE/CHAIRMAN/STOCK_RESERVATION/STOCK_PAYMENT/STOCK_WAREHOUSE/WAREHOUSE/PAYMENT/RECEIPT_EX/INVOICE_*
	AssigneeID   *string
	LegacyTaskID *string
	Deadline     *time.Time
}

// CreateApprovalTask 创建审批任务（同订单同 kind 去重：已存在 PENDING 则返回既有；
// ID 用随机后缀，裁决后同 kind 可再次创建——修改 A 类重审等场景）。
func (r *Repo) CreateApprovalTask(ctx context.Context, tx *Tx, in NewApprovalTask) (string, error) {
	// 去重：同订单同 kind 的 PENDING 任务已存在 → 返回既有（同事务可见性）
	var existing string
	if err := tx.tx.QueryRow(ctx, `
		SELECT id FROM approval_tasks WHERE tenant_id=$1 AND order_id=$2 AND kind=$3 AND state='PENDING'
		ORDER BY created_at LIMIT 1`, in.TenantID, in.OrderID, in.Kind).Scan(&existing); err == nil {
		return existing, nil
	}
	id := "AT-" + uuidSuffix()
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO approval_tasks (id, tenant_id, order_id, stage_code, kind, state, assignee_id, legacy_task_id, deadline)
		VALUES ($1,$2,$3,$4,$5,'PENDING',$6,$7,$8)`,
		id, in.TenantID, in.OrderID, in.StageCode, in.Kind, in.AssigneeID, in.LegacyTaskID, in.Deadline)
	if err != nil {
		return "", fmt.Errorf("insert approval_task: %w", err)
	}
	return id, nil
}

// ListApprovalTasks 待办检索（assignee/kind/state 组合过滤，L1-11）。
func (r *Repo) ListApprovalTasks(ctx context.Context, tenantID string, assignee, kind, state string, limit int) ([]ApprovalTask, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, order_id, stage_code, kind, state, assignee_id, legacy_task_id, deadline,
		       verdict, decided_by, decided_at, created_at
		FROM approval_tasks
		WHERE tenant_id=$1 AND ($2='' OR assignee_id=$2) AND ($3='' OR kind=$3) AND ($4='' OR state=$4)
		ORDER BY created_at LIMIT $5`, tenantID, assignee, kind, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalTask
	for rows.Next() {
		var t ApprovalTask
		if err := rows.Scan(&t.ID, &t.TenantID, &t.OrderID, &t.StageCode, &t.Kind, &t.State, &t.AssigneeID,
			&t.LegacyTaskID, &t.Deadline, &t.Verdict, &t.DecidedBy, &t.DecidedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DecideTask 审批裁决（PENDING → APPROVED/REJECTED/RETURNED/ESCALATED）+ 订单事件留痕。
// verdict 为审批意见；detail 携带裁决上下文。
func (r *Repo) DecideTask(ctx context.Context, tx *Tx, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error {
	var t ApprovalTask
	if err := tx.tx.QueryRow(ctx, `
		SELECT id, tenant_id, order_id, stage_code, kind, state, assignee_id, legacy_task_id, deadline,
		       verdict, decided_by, decided_at, created_at
		FROM approval_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, taskID).Scan(
		&t.ID, &t.TenantID, &t.OrderID, &t.StageCode, &t.Kind, &t.State, &t.AssigneeID,
		&t.LegacyTaskID, &t.Deadline, &t.Verdict, &t.DecidedBy, &t.DecidedAt, &t.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: "approval task " + taskID}
		}
		return err
	}
	if t.State != "PENDING" {
		return &ErrAlreadyExists{Msg: fmt.Sprintf("approval task %s already decided (%s)", taskID, t.State)}
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE approval_tasks SET state=$3, verdict=$4, decided_by=$5, decided_at=now()
		WHERE tenant_id=$1 AND id=$2`,
		tenantID, taskID, toState, verdict, decidedBy); err != nil {
		return err
	}
	action := "ApprovalDecided:" + t.Kind
	if toState == "ESCALATED" {
		action = "ApprovalEscalated"
	}
	return r.appendOrderEvent(ctx, tx, tenantID, t.OrderID, t.StageCode, &taskID, action,
		"", "", fmt.Sprintf("审批裁决 %s -> %s (verdict=%s)", t.Kind, toState, verdict), detail, nil, decidedBy)
}

// GetApprovalTask 查询单个任务。
func (r *Repo) GetApprovalTask(ctx context.Context, tenantID, taskID string) (*ApprovalTask, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, order_id, stage_code, kind, state, assignee_id, legacy_task_id, deadline,
		       verdict, decided_by, decided_at, created_at
		FROM approval_tasks WHERE tenant_id=$1 AND id=$2`, tenantID, taskID)
	var t ApprovalTask
	err := row.Scan(&t.ID, &t.TenantID, &t.OrderID, &t.StageCode, &t.Kind, &t.State, &t.AssigneeID,
		&t.LegacyTaskID, &t.Deadline, &t.Verdict, &t.DecidedBy, &t.DecidedAt, &t.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "approval task " + taskID}
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// uuidSuffix 随机短 ID 后缀。
func uuidSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ── 会话存档游标（L1-14；07 §2.3 原子 checkpoint）──

// GetArchiveCursor 读取游标。
func (r *Repo) GetArchiveCursor(ctx context.Context, tenantID string) (int64, error) {
	var seq int64
	err := r.pool.QueryRow(ctx, `SELECT seq FROM wecom_archive_cursor WHERE tenant_id=$1`, tenantID).Scan(&seq)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// AdvanceArchiveCursor 原子推进游标（UPSERT；中断/重启后不丢 checkpoint）。
// nextSeq 必须 ≥ 当前 seq（防回退）。
func (r *Repo) AdvanceArchiveCursor(ctx context.Context, tenantID string, nextSeq int64) error {
	cur, err := r.GetArchiveCursor(ctx, tenantID)
	if err != nil {
		return err
	}
	if nextSeq < cur {
		return fmt.Errorf("cursor regression: next %d < current %d", nextSeq, cur)
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO wecom_archive_cursor (tenant_id, seq, updated_at)
		VALUES ($1,$2,now())
		ON CONFLICT (tenant_id) DO UPDATE SET seq = EXCLUDED.seq, updated_at = now()
		WHERE wecom_archive_cursor.seq <= EXCLUDED.seq`,
		tenantID, nextSeq)
	if err != nil {
		// WHERE 不满足时不更新（无行受影响属正常）
		return err
	}
	return nil
}

// ── 事件链查询（CQ-12 审计回放）──

// ListOrderEvents 按订单回放事件链（分域聚合，按时间序）。
func (r *Repo) ListOrderEvents(ctx context.Context, tenantID, orderID string, limit int) ([]Event, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, 'orders'::text, order_id, order_id, stage_code, task_id, action,
		       from_state, to_state, reason, detail, error, operator_id, occurred_at
		FROM order_events WHERE tenant_id=$1 AND order_id=$2
		ORDER BY id LIMIT $3`, tenantID, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Aggregate, &e.AggregateID, &e.OrderID, &e.StageCode,
			&e.TaskID, &e.Action, &e.FromState, &e.ToState, &e.Reason, &e.Detail, &e.Error, &e.OperatorID, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListPreOrderEvents 预订单事件链回放。
func (r *Repo) ListPreOrderEvents(ctx context.Context, tenantID, preOrderID string) ([]Event, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, 'pre_orders'::text, pre_order_id, '', '', '', action,
		       from_state, to_state, reason, detail, error, operator_id, occurred_at
		FROM pre_order_events WHERE tenant_id=$1 AND pre_order_id=$2 ORDER BY id`, tenantID, preOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Aggregate, &e.AggregateID, &e.OrderID, &e.StageCode,
			&e.TaskID, &e.Action, &e.FromState, &e.ToState, &e.Reason, &e.Detail, &e.Error, &e.OperatorID, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
