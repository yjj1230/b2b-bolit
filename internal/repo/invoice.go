package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// ── 开票（INV）：单事务：开票守恒（金额+数量双维）→ 发票 + 明细 + 事件（防线 4/5；CQ-09）──

// ApplyInvoice 开票申请/确认。事务内校验（按 order_item 聚合有效 shipment_items）：
//
//	∑(开票数量) ≤ ∑(有效发货数量) - ∑(已开票数量)
//	∑(开票金额) ≤ ∑(有效发货金额) - ∑(已开票金额)      （金额 = 有效发货量 × 单价）
//
// 任一维度超限 → domain.ErrConservation，事务回滚。
func (r *Repo) ApplyInvoice(ctx context.Context, tx *Tx, inv Invoice, items []InvoiceItem, operator string) error {
	// 行锁订单（同一订单的开票串行化）
	if err := tx.tx.QueryRow(ctx,
		`SELECT id FROM orders WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
		inv.TenantID, inv.OrderID).Scan(&inv.OrderID); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: "order " + inv.OrderID}
		}
		return err
	}

	// 每明细双维守恒
	for _, it := range items {
		// 单价（守恒基准：order_items.unit_price_cents）
		var unitPrice int64
		if err := tx.tx.QueryRow(ctx,
			`SELECT unit_price_cents FROM order_items WHERE tenant_id=$1 AND id=$2`,
			it.TenantID, it.OrderItemID).Scan(&unitPrice); err != nil {
			if err == pgx.ErrNoRows {
				return &ErrNotFound{What: "order_item " + it.OrderItemID}
			}
			return err
		}
		var shippedQty, invoicedQty, invoicedAmount int64
		// 有效发货量（该明细）
		if err := tx.tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(si.qty),0) FROM shipment_items si
			JOIN shipments s ON s.id = si.shipment_id
			WHERE s.tenant_id=$1 AND s.order_id=$2 AND si.order_item_id=$3 AND s.state IN ('SHIPPED','CLOSED')`,
			it.TenantID, inv.OrderID, it.OrderItemID).Scan(&shippedQty); err != nil {
			return err
		}
		// 已开票量/金额（该明细，跨发票聚合）
		if err := tx.tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(ii.qty),0), COALESCE(SUM(ii.amount_cents),0)
			FROM invoice_items ii JOIN invoices i ON i.id = ii.invoice_id
			WHERE i.tenant_id=$1 AND i.order_id=$2 AND ii.order_item_id=$3`,
			it.TenantID, inv.OrderID, it.OrderItemID).Scan(&invoicedQty, &invoicedAmount); err != nil {
			return err
		}
		availableQty := shippedQty - invoicedQty
		availableAmount := shippedQty*unitPrice - invoicedAmount
		if err := domain.AssertInvoiceWithinShipped(it.Qty, it.AmountCents, availableQty, availableAmount); err != nil {
			return err
		}
	}

	// A4 订单级关联守恒：本次开票金额/数量 ≤ 订单未开票额度（links 为额度分配事实源）
	var linkedCents, linkedQty int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(link.amount_cents),0), COALESCE(SUM(link.qty),0)
		FROM invoice_order_links link
		JOIN invoices i ON i.id = link.invoice_id AND i.tenant_id = $1
		WHERE link.tenant_id=$1 AND link.order_id=$2`,
		inv.TenantID, inv.OrderID).Scan(&linkedCents, &linkedQty); err != nil {
		return err
	}
	var orderShippedCents, orderShippedQty int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.qty*oi.unit_price_cents),0), COALESCE(SUM(si.qty),0)
		FROM shipment_items si
		JOIN shipments s ON s.id=si.shipment_id AND s.tenant_id=$1
		JOIN order_items oi ON oi.id=si.order_item_id
		WHERE s.order_id=$2 AND s.state IN ('SHIPPED','CLOSED')`,
		inv.TenantID, inv.OrderID).Scan(&orderShippedCents, &orderShippedQty); err != nil {
		return err
	}
	if err := domain.AssertInvoiceWithinShipped(inv.Qty, inv.AmountCents,
		orderShippedQty-linkedQty, orderShippedCents-linkedCents); err != nil {
		return err
	}
	// 三金额快照（A4）：发货额 = 订单有效发货金额；开票额 = 本次开票金额；收款额 0（回款登记后更新）
	var shippedCents int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.qty * oi.unit_price_cents), 0)
		FROM shipment_items si
		JOIN shipments s ON s.id = si.shipment_id AND s.tenant_id = $1
		JOIN order_items oi ON oi.id = si.order_item_id
		WHERE s.order_id = $2 AND s.state IN ('SHIPPED','CLOSED')`,
		inv.TenantID, inv.OrderID).Scan(&shippedCents); err != nil {
		return err
	}
	// 插入发票 + 明细（幂等：同键重放 → 跳过，防线 3/7）
	ct, err := tx.tx.Exec(ctx, `
		INSERT INTO invoices (id, tenant_id, order_id, state, amount_cents, qty, idempotency_key,
		                      received_cents, shipped_cents, invoiced_cents)
		VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$5)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		inv.ID, inv.TenantID, inv.OrderID, inv.State, inv.AmountCents, inv.Qty, inv.IdempotencyKey, shippedCents)
	if err != nil {
		return fmt.Errorf("insert invoice: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return nil // 幂等重放
	}
	for _, it := range items {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO invoice_items (id, tenant_id, invoice_id, order_item_id, product_id, qty, amount_cents, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			it.ID, it.TenantID, it.InvoiceID, it.OrderItemID, it.ProductID, it.Qty, it.AmountCents, it.IdempotencyKey); err != nil {
			return fmt.Errorf("insert invoice_item: %w", err)
		}
	}
	// 主关联（发票 ↔ 主订单；A4 关联可追加，后续经 LinkInvoiceOrderTx 补关联其他订单）
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO invoice_order_links (tenant_id, invoice_id, order_id, amount_cents, qty)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, invoice_id, order_id) DO NOTHING`,
		inv.TenantID, inv.ID, inv.OrderID, inv.AmountCents, inv.Qty); err != nil {
		return fmt.Errorf("insert invoice_order_link: %w", err)
	}
	// 事件链（append-only）
	return r.appendInvoiceEvent(ctx, tx, inv.TenantID, inv.ID, "", "", inv.OrderID, "INV", nil,
		"InvoiceIssued", "", inv.State, "开票完成（守恒校验通过）", nil, nil, operator)
}

// GetInvoice 查询发票。
func (r *Repo) GetInvoice(ctx context.Context, tenantID, invoiceID string) (*Invoice, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, order_id, state, amount_cents, qty, idempotency_key, created_at, updated_at,
		       received_cents, shipped_cents, invoiced_cents
		FROM invoices WHERE tenant_id=$1 AND id=$2`, tenantID, invoiceID)
	var v Invoice
	err := row.Scan(&v.ID, &v.TenantID, &v.OrderID, &v.State, &v.AmountCents, &v.Qty, &v.IdempotencyKey,
		&v.CreatedAt, &v.UpdatedAt, &v.ReceivedCents, &v.ShippedCents, &v.InvoicedCents)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "invoice " + invoiceID}
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *Repo) appendInvoiceEvent(ctx context.Context, tx *Tx, tenantID, invoiceID, shipmentID, stockPrepareID, orderID, stage string, taskID *string, action, fromState, toState, reason string, detail []byte, errStr *string, operator string) error {
	var errS, op, ship, sp *string
	if errStr != nil {
		errS = errStr
	}
	if operator != "" {
		op = &operator
	}
	if shipmentID != "" {
		ship = &shipmentID
	}
	if stockPrepareID != "" {
		sp = &stockPrepareID
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO invoice_events (tenant_id, invoice_id, shipment_id, stock_prepare_id, order_id, stage_code,
		                            task_id, action, from_state, to_state, reason, detail, error, operator_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		tenantID, invoiceID, ship, sp, orderID, stage, taskID, action, fromState, toState, reason, detail, errS, op)
	return err
}
