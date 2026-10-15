package repo

import (
	"context"
	"fmt"
	"time"
)

type NewWorkflowOutboxEvent struct {
	TenantID    string
	EventKey    string
	EventType   string
	AggregateID string
	WorkflowID  string
	Payload     []byte
}

// EnqueueWorkflowOutboxTx 在已有业务事务中写入 workflow outbox。
func (r *Repo) EnqueueWorkflowOutboxTx(ctx context.Context, tx *Tx, in NewWorkflowOutboxEvent) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO workflow_outbox_events (tenant_id, event_key, event_type, aggregate_id, workflow_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant_id, event_key) DO NOTHING`,
		in.TenantID, in.EventKey, in.EventType, in.AggregateID, in.WorkflowID, jsonOrEmpty(in.Payload, "{}"))
	return err
}

func (r *Repo) EnqueueWorkflowOutbox(ctx context.Context, in NewWorkflowOutboxEvent) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.EnqueueWorkflowOutboxTx(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimWorkflowOutbox 租约式领取待派发事件。
func (r *Repo) ClaimWorkflowOutbox(ctx context.Context, limit int, lease time.Duration) ([]WorkflowOutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.tx.Query(ctx, `
		WITH picked AS (
			SELECT id FROM workflow_outbox_events
			WHERE (status='PENDING' OR (status='PROCESSING' AND locked_until < now()))
			  AND available_at <= now()
			ORDER BY id
			FOR UPDATE SKIP LOCKED LIMIT $1
		)
		UPDATE workflow_outbox_events e
		SET status='PROCESSING', attempts=e.attempts+1, locked_until=now()+$2::interval
		FROM picked p WHERE e.id=p.id
		RETURNING e.id, e.tenant_id, e.event_key, e.event_type, e.aggregate_id, e.workflow_id,
		          e.payload, e.status, e.attempts, e.available_at, e.locked_until, e.last_error,
		          e.created_at, e.processed_at`, limit, fmt.Sprintf("%f seconds", lease.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowOutboxEvent
	for rows.Next() {
		var e WorkflowOutboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventKey, &e.EventType, &e.AggregateID, &e.WorkflowID,
			&e.Payload, &e.Status, &e.Attempts, &e.AvailableAt, &e.LockedUntil, &e.LastError, &e.CreatedAt, &e.ProcessedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) MarkWorkflowOutboxDone(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE workflow_outbox_events SET status='DONE', processed_at=now(), locked_until=NULL WHERE id=$1`, id)
	return err
}

func (r *Repo) RetryWorkflowOutbox(ctx context.Context, id int64, retryAt time.Time, lastErr string) error {
	_, err := r.pool.Exec(ctx, `UPDATE workflow_outbox_events SET status='PENDING', available_at=$2, locked_until=NULL, last_error=$3 WHERE id=$1`, id, retryAt, lastErr)
	return err
}

func (r *Repo) DeadLetterWorkflowOutbox(ctx context.Context, e WorkflowOutboxEvent, lastErr string) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.tx.Exec(ctx, `UPDATE workflow_outbox_events SET status='DEAD', locked_until=NULL, last_error=$2 WHERE id=$1`, e.ID, lastErr); err != nil {
		return err
	}
	if _, err := tx.tx.Exec(ctx, `INSERT INTO dead_letter_events (tenant_id, source, source_id, payload, error, attempts) VALUES ($1,$2,$3,$4,$5,$6)`,
		e.TenantID, "workflow_outbox", fmt.Sprint(e.ID), jsonOrEmpty(e.Payload, "{}"), lastErr, e.Attempts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) GetWorkflowOutboxByAggregate(ctx context.Context, tenantID, aggregateID string) ([]WorkflowOutboxEvent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, event_key, event_type, aggregate_id, workflow_id, payload, status, attempts,
		       available_at, locked_until, last_error, created_at, processed_at
		FROM workflow_outbox_events WHERE tenant_id=$1 AND aggregate_id=$2 ORDER BY id`, tenantID, aggregateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowOutboxEvent
	for rows.Next() {
		var e WorkflowOutboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventKey, &e.EventType, &e.AggregateID, &e.WorkflowID,
			&e.Payload, &e.Status, &e.Attempts, &e.AvailableAt, &e.LockedUntil, &e.LastError, &e.CreatedAt, &e.ProcessedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
