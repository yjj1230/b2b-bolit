package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// ── 预订单（企微 S2；CQ-11 消息幂等 + 订单幂等双层）──

// NewPreOrder 预订单创建请求。
type NewPreOrder struct {
	TenantID       string
	GroupID        string
	MsgID          string
	Candidates     []byte
	IdempotencyKey string
}

// CreatePreOrder 消息幂等创建预订单（防线 3：UNIQUE(tenant_id, group_id, msg_id)）。
// 同一消息重复到达 → 返回既有预订单（created=false）；不同消息同键 → ErrAlreadyExists。
func (r *Repo) CreatePreOrder(ctx context.Context, in NewPreOrder) (po *PreOrder, created bool, err error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	id := "PRE-" + shortID(in.TenantID+":"+in.IdempotencyKey)
	row := tx.tx.QueryRow(ctx, `
		INSERT INTO pre_orders (id, tenant_id, group_id, msg_id, state, candidates, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id, group_id, msg_id) DO NOTHING
		RETURNING id`,
		id, in.TenantID, in.GroupID, in.MsgID, domain.StatePreCaptured, in.Candidates, in.IdempotencyKey)
	err = row.Scan(&id)
	if err == pgx.ErrNoRows {
		existing, gerr := r.GetPreOrderByMsg(ctx, in.TenantID, in.GroupID, in.MsgID)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("insert pre_order: %w", err)
	}
	// 预订单事件链（append-only）
	if err := r.appendPreOrderEvent(ctx, tx, in.TenantID, id, "", domain.StatePreCaptured,
		"PreOrderCaptured", "会话存档拉到消息", in.Candidates, nil, ""); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	po, err = r.GetPreOrder(ctx, in.TenantID, id)
	return po, true, err
}

// GetPreOrder 查询预订单。
func (r *Repo) GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*PreOrder, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, group_id, msg_id, state, candidates, idempotency_key, created_at, updated_at
		FROM pre_orders WHERE tenant_id=$1 AND id=$2`, tenantID, preOrderID)
	return scanPreOrder(row)
}

// GetPreOrderByMsg 按消息幂等键查询预订单。
func (r *Repo) GetPreOrderByMsg(ctx context.Context, tenantID, groupID, msgID string) (*PreOrder, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, group_id, msg_id, state, candidates, idempotency_key, created_at, updated_at
		FROM pre_orders WHERE tenant_id=$1 AND group_id=$2 AND msg_id=$3`, tenantID, groupID, msgID)
	return scanPreOrder(row)
}

func scanPreOrder(row rowScanner) (*PreOrder, error) {
	var p PreOrder
	err := row.Scan(&p.ID, &p.TenantID, &p.GroupID, &p.MsgID, &p.State, &p.Candidates, &p.IdempotencyKey, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "pre_order"}
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdatePreOrderState 预订单状态迁移 + 事件同事务（FSM 校验由调用方完成）。
func (r *Repo) UpdatePreOrderState(ctx context.Context, tx *Tx, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error {
	if err := tx.tx.QueryRow(ctx,
		`UPDATE pre_orders SET state=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2 AND state=$4 RETURNING id`,
		tenantID, preOrderID, toState, fromState).Scan(&preOrderID); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: fmt.Sprintf("pre_order %s in state %s", preOrderID, fromState)}
		}
		return err
	}
	return r.appendPreOrderEvent(ctx, tx, tenantID, preOrderID, fromState, toState, action, reason, detail, nil, operator)
}

func (r *Repo) appendPreOrderEvent(ctx context.Context, tx *Tx, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, errStr *string, operator string) error {
	var errS, op *string
	if errStr != nil {
		errS = errStr
	}
	if operator != "" {
		op = &operator
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO pre_order_events (tenant_id, pre_order_id, action, from_state, to_state, reason, detail, error, operator_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		tenantID, preOrderID, action, fromState, toState, reason, detail, errS, op)
	return err
}

// ListPreOrders 列出预订单（待办/测试）。
func (r *Repo) ListPreOrders(ctx context.Context, tenantID string, limit int) ([]PreOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, group_id, msg_id, state, candidates, idempotency_key, created_at, updated_at
		FROM pre_orders WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreOrder
	for rows.Next() {
		var p PreOrder
		if err := rows.Scan(&p.ID, &p.TenantID, &p.GroupID, &p.MsgID, &p.State, &p.Candidates, &p.IdempotencyKey, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
