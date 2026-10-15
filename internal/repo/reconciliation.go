package repo

import (
	"context"
	"time"
)

// FindRequestedOrderOrphans 找出已写入 ORD_REQUESTED 但没有可继续派发 outbox 的订单。
func (r *Repo) FindRequestedOrderOrphans(ctx context.Context, tenantID string, olderThan time.Duration) ([]Order, error) {
	if olderThan <= 0 {
		olderThan = 5 * time.Minute
	}
	rows, err := r.pool.Query(ctx, `
		SELECT o.id, o.tenant_id, o.customer_id, o.state, o.payment_term, o.supplier_channel, o.contract_no,
		       o.idempotency_key, o.payload_hash, o.payload, o.legacy_flow_id, o.submission_mode,
		       o.resubmit_from_order_id, o.resubmit_root_order_id, o.resubmit_revision, o.created_at, o.updated_at
		FROM orders o
		WHERE ($1='' OR o.tenant_id=$1) AND o.state='ORD_REQUESTED'
		  AND o.created_at < now() - $2::interval
		  AND NOT EXISTS (
			SELECT 1 FROM workflow_outbox_events w
			WHERE w.tenant_id=o.tenant_id AND w.aggregate_id=o.id AND w.status IN ('PENDING','PROCESSING','DONE')
		  )
		ORDER BY o.created_at`, tenantID, olderThan.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.State, &o.PaymentTerm, &o.SupplierChannel,
			&o.ContractNo, &o.IdempotencyKey, &o.PayloadHash, &o.Payload, &o.LegacyFlowID, &o.SubmissionMode,
			&o.ResubmitFromOrderID, &o.ResubmitRootOrderID, &o.ResubmitRevision, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
