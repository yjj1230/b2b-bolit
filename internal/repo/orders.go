package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// HashPayload 计算 payload 指纹（幂等同键同 payload 判定，架构 §5 层 1）。
func HashPayload(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// NewOrder 快速下单请求（API/脚本入口）。
type NewOrder struct {
	TenantID            string
	CustomerID          string
	PaymentTerm         string
	SupplierChannel     string
	IdempotencyKey      string
	Payload             []byte
	Items               []NewOrderItem
	SubmissionMode      string
	ResubmitFromOrderID *string
	ResubmitRootOrderID *string
	ResubmitRevision    int
	LegacyFlowID        *string
	AgentTrace          *OrderCreationTrace
}

// NewOrderItem 下单明细。
type NewOrderItem struct {
	ID             string
	ProductID      string
	WarehouseID    *string
	Quantity       int64
	UnitPriceCents int64
	// ReceiveAddress 收货地址（A1：商务备货按收货地址选仓；可空）。
	ReceiveAddress string
}

// OrderCreationTrace 用于把 Agent 草稿和确认审计关联到正式订单。
type OrderCreationTrace struct {
	PreOrderID    string
	DraftVersion  int64
	ConfirmedBy   string
	CorrelationID string
}

// CreateOrderWithWorkflowOutbox 在订单、明细、OrderRequested 事件和 workflow outbox
// 同一事务中提交。Temporal 启动由 dispatcher 异步完成。
func (r *Repo) CreateOrderWithWorkflowOutbox(ctx context.Context, in NewOrder, trace OrderCreationTrace) (ord *Order, created bool, err error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	hash := HashPayload(in.Payload)
	id := "ORD-" + shortID(in.TenantID+":"+in.IdempotencyKey)
	mode := in.SubmissionMode
	if mode == "" {
		mode = "create"
	}
	row := tx.tx.QueryRow(ctx, `
		INSERT INTO orders (id, tenant_id, customer_id, state, payment_term, supplier_channel,
		                    idempotency_key, payload_hash, payload, submission_mode,
		                    resubmit_from_order_id, resubmit_root_order_id, resubmit_revision, legacy_flow_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		RETURNING id`,
		id, in.TenantID, in.CustomerID, domain.StateORDRequested, in.PaymentTerm, in.SupplierChannel,
		in.IdempotencyKey, hash, in.Payload, mode, in.ResubmitFromOrderID, in.ResubmitRootOrderID,
		in.ResubmitRevision, in.LegacyFlowID)
	if err := row.Scan(&id); err == pgx.ErrNoRows {
		existing, gerr := r.GetOrderByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey)
		if gerr != nil {
			return nil, false, gerr
		}
		if existing.PayloadHash != hash {
			return nil, false, &ErrIdempotencyConflict{Key: in.IdempotencyKey}
		}
		return existing, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("insert order: %w", err)
	}
	for _, it := range in.Items {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO order_items (id, tenant_id, order_id, product_id, warehouse_id, quantity, unit_price_cents, receive_address)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			it.ID, in.TenantID, id, it.ProductID, it.WarehouseID, it.Quantity, it.UnitPriceCents, it.ReceiveAddress); err != nil {
			return nil, false, fmt.Errorf("insert order_item: %w", err)
		}
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if trace.PreOrderID != "" {
		payload = MarshalJSONDetail(map[string]any{
			"pre_order_id": trace.PreOrderID, "draft_version": trace.DraftVersion,
			"confirmed_by": trace.ConfirmedBy, "correlation_id": trace.CorrelationID,
			"order_payload": json.RawMessage(payload),
		})
		if _, err := tx.tx.Exec(ctx, `UPDATE orders SET payload=$3, payload_hash=$4 WHERE tenant_id=$1 AND id=$2`, in.TenantID, id, payload, HashPayload(payload)); err != nil {
			return nil, false, err
		}
	}
	if err := r.appendOrderEvent(ctx, tx, in.TenantID, id, "ORD", nil, "OrderRequested", "", domain.StateORDRequested, "预订单确认派生正式订单", payload, nil, trace.ConfirmedBy); err != nil {
		return nil, false, err
	}
	workflowID := "order-" + in.TenantID + "-" + in.IdempotencyKey
	if err := r.EnqueueWorkflowOutboxTx(ctx, tx, NewWorkflowOutboxEvent{
		TenantID: in.TenantID, EventKey: in.TenantID + ":" + in.IdempotencyKey,
		EventType: "order.workflow.start", AggregateID: id, WorkflowID: workflowID,
		Payload: MarshalJSONDetail(map[string]any{"tenant_id": in.TenantID, "order_id": id, "workflow_id": workflowID}),
	}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	ord, err = r.GetOrder(ctx, in.TenantID, id)
	return ord, true, err
}

// CreateOrder 幂等创建订单（防线 3：UNIQUE(tenant_id, idempotency_key)）。
// 同键同 payload → 返回既有订单（created=false）；同键异 payload → ErrIdempotencyConflict。
// 订单 + 明细 + OrderRequested 事件同事务（防线 4/6）。
func (r *Repo) CreateOrder(ctx context.Context, in NewOrder) (ord *Order, created bool, err error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	hash := HashPayload(in.Payload)
	id := "ORD-" + shortID(in.TenantID+":"+in.IdempotencyKey)
	mode := in.SubmissionMode
	if mode == "" {
		mode = "create"
	}
	row := tx.tx.QueryRow(ctx, `
		INSERT INTO orders (id, tenant_id, customer_id, state, payment_term, supplier_channel,
		                    idempotency_key, payload_hash, payload, submission_mode,
		                    resubmit_from_order_id, resubmit_root_order_id, resubmit_revision, legacy_flow_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		RETURNING id, state`,
		id, in.TenantID, in.CustomerID, domain.StateORDRequested, in.PaymentTerm, in.SupplierChannel,
		in.IdempotencyKey, hash, in.Payload, mode,
		in.ResubmitFromOrderID, in.ResubmitRootOrderID, in.ResubmitRevision, in.LegacyFlowID)
	var state string
	err = row.Scan(&id, &state)
	if err == pgx.ErrNoRows {
		// 幂等键已存在：回查并比对 payload
		existing, gerr := r.GetOrderByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey)
		if gerr != nil {
			return nil, false, gerr
		}
		if existing.PayloadHash != hash {
			return nil, false, &ErrIdempotencyConflict{Key: in.IdempotencyKey}
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("insert order: %w", err)
	}

	for _, it := range in.Items {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO order_items (id, tenant_id, order_id, product_id, warehouse_id, quantity, unit_price_cents, receive_address)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			it.ID, in.TenantID, id, it.ProductID, it.WarehouseID, it.Quantity, it.UnitPriceCents, it.ReceiveAddress); err != nil {
			return nil, false, fmt.Errorf("insert order_item: %w", err)
		}
	}
	if err := r.appendOrderEvent(ctx, tx, in.TenantID, id, "ORD", nil, "OrderRequested",
		"", domain.StateORDRequested, "快速下单/预订单确认", in.Payload, nil, ""); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	ord, err = r.GetOrder(ctx, in.TenantID, id)
	return ord, true, err
}

// GetOrder 按 ID 查询订单。
func (r *Repo) GetOrder(ctx context.Context, tenantID, orderID string) (*Order, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, customer_id, state, payment_term, supplier_channel, contract_no,
		       idempotency_key, payload_hash, payload, legacy_flow_id, submission_mode,
		       resubmit_from_order_id, resubmit_root_order_id, resubmit_revision, created_at, updated_at
		FROM orders WHERE tenant_id=$1 AND id=$2`, tenantID, orderID)
	return scanOrder(row)
}

// GetOrderByIdempotencyKey 按幂等键查询订单。
func (r *Repo) GetOrderByIdempotencyKey(ctx context.Context, tenantID, key string) (*Order, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, customer_id, state, payment_term, supplier_channel, contract_no,
		       idempotency_key, payload_hash, payload, legacy_flow_id, submission_mode,
		       resubmit_from_order_id, resubmit_root_order_id, resubmit_revision, created_at, updated_at
		FROM orders WHERE tenant_id=$1 AND idempotency_key=$2`, tenantID, key)
	return scanOrder(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanOrder(row rowScanner) (*Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.State, &o.PaymentTerm, &o.SupplierChannel,
		&o.ContractNo, &o.IdempotencyKey, &o.PayloadHash, &o.Payload, &o.LegacyFlowID, &o.SubmissionMode,
		&o.ResubmitFromOrderID, &o.ResubmitRootOrderID, &o.ResubmitRevision, &o.CreatedAt, &o.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "order " + o.ID}
	}
	if err != nil {
		return nil, fmt.Errorf("scan order: %w", err)
	}
	return &o, nil
}

// GetOrderItems 查询订单明细。
func (r *Repo) GetOrderItems(ctx context.Context, tenantID, orderID string) ([]OrderItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, order_id, product_id, warehouse_id, quantity, unit_price_cents, receive_address
		FROM order_items WHERE tenant_id=$1 AND order_id=$2 ORDER BY id`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []OrderItem
	for rows.Next() {
		var it OrderItem
		if err := rows.Scan(&it.ID, &it.TenantID, &it.OrderID, &it.ProductID, &it.WarehouseID, &it.Quantity, &it.UnitPriceCents, &it.ReceiveAddress); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// UpdateOrderState 状态迁移 + 事件链同事务（防线 4/6；FSM 校验前置由调用方完成）。
func (r *Repo) UpdateOrderState(ctx context.Context, tx *Tx, tenantID, orderID, fromState, toState, action, operator, reason string, detail []byte) error {
	if err := tx.tx.QueryRow(ctx,
		`UPDATE orders SET state=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2 AND state=$4 RETURNING id`,
		tenantID, orderID, toState, fromState).Scan(&orderID); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: fmt.Sprintf("order %s in state %s", orderID, fromState)}
		}
		return err
	}
	return r.appendOrderEvent(ctx, tx, tenantID, orderID, domain.Stage(toState), nil, action, fromState, toState, reason, detail, nil, operator)
}

// UpdateOrderMeta 更新订单元信息（合同号/供方等），不触发状态迁移（E 类静默修改）。
func (r *Repo) UpdateOrderMeta(ctx context.Context, tx *Tx, tenantID, orderID, contractNo string, payload []byte) error {
	_, err := tx.tx.Exec(ctx,
		`UPDATE orders SET contract_no=$3, payload=$4, payload_hash=$5, updated_at=now() WHERE tenant_id=$1 AND id=$2`,
		tenantID, orderID, contractNo, payload, HashPayload(payload))
	return err
}

// SetOrderContract 合同生成成功写入合同号。
func (r *Repo) SetOrderContract(ctx context.Context, tx *Tx, tenantID, orderID, contractNo string) error {
	_, err := tx.tx.Exec(ctx,
		`UPDATE orders SET contract_no=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2`,
		tenantID, orderID, contractNo)
	return err
}

// appendOrderEvent 追加订单事件（append-only，防线 6）。
func (r *Repo) appendOrderEvent(ctx context.Context, tx *Tx, tenantID, orderID, stage string, taskID *string, action, fromState, toState, reason string, detail []byte, errStr *string, operator string) error {
	var errS *string
	if errStr != nil {
		errS = errStr
	}
	var op *string
	if operator != "" {
		op = &operator
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO order_events (tenant_id, order_id, stage_code, task_id, action, from_state, to_state, reason, detail, error, operator_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		tenantID, orderID, stage, taskID, action, fromState, toState, reason, detail, errS, op)
	return err
}

// ListOrders 按租户+状态列出订单（待办/对账用）。
func (r *Repo) ListOrders(ctx context.Context, tenantID, state string, limit int) ([]Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, customer_id, state, payment_term, supplier_channel, contract_no,
		       idempotency_key, payload_hash, payload, legacy_flow_id, submission_mode,
		       resubmit_from_order_id, resubmit_root_order_id, resubmit_revision, created_at, updated_at
		FROM orders WHERE ($1='' OR tenant_id=$1) AND ($2='' OR state=$2) ORDER BY created_at DESC LIMIT $3`,
		tenantID, state, limit)
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

// shortID 由幂等键派生确定性短 ID（可读且稳定）。
func shortID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// MarshalJSONDetail 便捷序列化（detail 字段）。
func MarshalJSONDetail(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
