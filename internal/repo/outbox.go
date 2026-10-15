package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── Outbox 投影事件（决策 D4；docs/02-architecture.html §2.3；场景 G/H 前提）──

// OutboxEvent 投影事件。
type OutboxEvent struct {
	ID          int64
	TenantID    string
	Aggregate   string
	AggregateID string
	EventType   string
	Payload     []byte
	Status      string
	CreatedAt   time.Time
}

// AppendOutboxEvent 事务内追加投影事件（Transactional Outbox：与业务数据同事务）。
func (r *Repo) AppendOutboxEvent(ctx context.Context, tx *Tx, tenantID, aggregate, aggregateID, eventType string, payload []byte) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO outbox_events (tenant_id, aggregate, aggregate_id, event_type, payload, status)
		VALUES ($1,$2,$3,$4,$5,'PENDING')`,
		tenantID, aggregate, aggregateID, eventType, payload)
	if err != nil {
		return fmt.Errorf("append outbox event: %w", err)
	}
	return nil
}

// ListPendingOutboxEvents 拉取待投影事件（PENDING，按 id 升序）。
func (r *Repo) ListPendingOutboxEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, aggregate, aggregate_id, event_type, payload, status, created_at
		FROM outbox_events WHERE status='PENDING' ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Aggregate, &e.AggregateID, &e.EventType, &e.Payload, &e.Status, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkOutboxProcessed 幂等标记已投影（PENDING → PROCESSED；重复调用不报错）。
func (r *Repo) MarkOutboxProcessed(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE outbox_events SET status='PROCESSED', processed_at=now()
		WHERE id=$1 AND status='PENDING'`, id)
	return err
}

// RequeueFailedOutboxEvents 将 FAILED 事件重新置为 PENDING（失败重放；场景 G/H 可靠性承诺）。
func (r *Repo) RequeueFailedOutboxEvents(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE outbox_events SET status='PENDING', processed_at=NULL WHERE status='FAILED'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MarkOutboxFailed 标记投影失败（可重放）。
func (r *Repo) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE outbox_events SET status='FAILED', processed_at=now()
		WHERE id=$1 AND status='PENDING'`, id)
	_ = errMsg
	return err
}

// ── 业务写入点（场景 H：客户联系人变更；场景 G：代办授权）──

// SetCustomerContactTx 客户联系人变更（场景 H）：
// 同事务更新 customer_contacts 事实表 + 追加 contact_upsert 投影事件（Delete 旧 + Write 新）。
// 返回旧联系人（供投影器删除）。
func (r *Repo) SetCustomerContactTx(ctx context.Context, tenantID, customerID, newContact string) (oldContact string, err error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	// 旧联系人（首建为空）
	oldContact = ""
	if err := tx.tx.QueryRow(ctx,
		`SELECT user_id FROM customer_contacts WHERE tenant_id=$1 AND customer_id=$2`,
		tenantID, customerID).Scan(&oldContact); err != nil && err != pgx.ErrNoRows {
		return "", fmt.Errorf("read customer contact: %w", err)
	}
	// UPSERT 新联系人
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO customer_contacts (tenant_id, customer_id, user_id) VALUES ($1,$2,$3)
		ON CONFLICT (tenant_id, customer_id)
		DO UPDATE SET user_id = EXCLUDED.user_id, updated_at = now()`,
		tenantID, customerID, newContact); err != nil {
		return "", fmt.Errorf("set customer contact: %w", err)
	}
	payload := MarshalJSONDetail(map[string]any{
		"customer_id": customerID, "old_contact": oldContact, "new_contact": newContact,
	})
	if err := r.AppendOutboxEvent(ctx, tx, tenantID, "customer", customerID, "customer.contact_upsert", payload); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return oldContact, nil
}

// GetCustomerContact 查询客户联系人（场景 D/H 判定数据源）。
func (r *Repo) GetCustomerContact(ctx context.Context, tenantID, customerID string) (string, error) {
	var userID string
	err := r.pool.QueryRow(ctx,
		`SELECT user_id FROM customer_contacts WHERE tenant_id=$1 AND customer_id=$2`,
		tenantID, customerID).Scan(&userID)
	if err == pgx.ErrNoRows {
		return "", &ErrNotFound{What: "customer contact " + customerID}
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// GrantDelegationTx 临时代办授权（场景 G）：追加 delegation.grant 投影事件
// （投影器写带 expiry_ok caveat 的 delegate 关系；TTL 到期由 Revoke 删除元组）。
func (r *Repo) GrantDelegationTx(ctx context.Context, tenantID, resourceType, resourceID, delegateID string, expiresAt time.Time) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	payload := MarshalJSONDetail(map[string]any{
		"resource_type": resourceType, "resource_id": resourceID,
		"delegate_id": delegateID, "expires_at": expiresAt.Format(time.RFC3339),
	})
	if err := r.AppendOutboxEvent(ctx, tx, tenantID, "delegation", resourceID, "delegation.grant", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeDelegationTx 撤销临时代办（场景 G：TTL 到期/提前收回）：追加 delegation.revoke 投影事件。
func (r *Repo) RevokeDelegationTx(ctx context.Context, tenantID, resourceType, resourceID, delegateID string) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	payload := MarshalJSONDetail(map[string]any{
		"resource_type": resourceType, "resource_id": resourceID, "delegate_id": delegateID,
	})
	if err := r.AppendOutboxEvent(ctx, tx, tenantID, "delegation", resourceID, "delegation.revoke", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
