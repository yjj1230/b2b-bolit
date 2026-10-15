package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// jsonUnmarshal JSON 解析辅助（与 workflows 包同名工具对齐）。
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ── 0.1 发布版新增仓储方法（docs/v0.1/07-database.html / 08-api.html）──
//  守门员确认事件（A3/N-01）· 发票-订单关联与三金额（A4/N-05）· 凭证金额守恒（A2/N-06）·
//  仓库群绑定（A6/N-10）——全部走既有防线（行锁/幂等唯一/单事务）。

// ShipmentShippedAmountCents 单张发货单已发货金额（∑ 发货明细 qty × 订单明细单价）。
// 凭证金额守恒校验基准（N-06：开票金额 = 已发货金额，超限拒绝）。
func (r *Repo) ShipmentShippedAmountCents(ctx context.Context, tenantID, shipmentID string) (int64, error) {
	var amt int64
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.qty * oi.unit_price_cents), 0)
		FROM shipment_items si
		JOIN shipments s ON s.id = si.shipment_id AND s.tenant_id = $1
		JOIN order_items oi ON oi.id = si.order_item_id
		WHERE si.shipment_id = $2 AND s.state IN ('SHIPPED','CLOSED')`,
		tenantID, shipmentID).Scan(&amt); err != nil {
		return 0, err
	}
	return amt, nil
}

// OrderThreeAmounts 订单三金额分展（A4：收款/发货/开票分别展示；未开票额度保留在订单上）。
// 收款 = PaymentTracked 事件聚合；发货 = 有效发货金额；开票 = invoice_order_links 聚合（跨发票/跨关联）。
func (r *Repo) OrderThreeAmounts(ctx context.Context, tenantID, orderID string) (*OrderThreeAmounts, error) {
	var exists string
	if err := r.pool.QueryRow(ctx, `SELECT id FROM orders WHERE tenant_id=$1 AND id=$2`, tenantID, orderID).Scan(&exists); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &ErrNotFound{What: "order " + orderID}
		}
		return nil, err
	}
	out := &OrderThreeAmounts{OrderID: orderID}
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((detail->>'amount')::bigint), 0)
		FROM order_events WHERE tenant_id=$1 AND order_id=$2 AND action='PaymentTracked'`,
		tenantID, orderID).Scan(&out.ReceivedCents); err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.qty * oi.unit_price_cents), 0)
		FROM shipment_items si
		JOIN shipments s ON s.id = si.shipment_id AND s.tenant_id = $1
		JOIN order_items oi ON oi.id = si.order_item_id
		WHERE s.order_id = $2 AND s.state IN ('SHIPPED','CLOSED')`,
		tenantID, orderID).Scan(&out.ShippedCents); err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(link.amount_cents), 0)
		FROM invoice_order_links link
		JOIN invoices i ON i.id = link.invoice_id AND i.tenant_id = $1
		WHERE link.tenant_id = $1 AND link.order_id = $2`,
		tenantID, orderID).Scan(&out.InvoicedCents); err != nil {
		return nil, err
	}
	out.UninvoicedCents = out.ShippedCents - out.InvoicedCents
	return out, nil
}

// LinkInvoiceOrderTx 追加发票-订单关联（A4：关联可追加，漏了可补关联第 N 笔）。
// 守恒（订单级双维）：本次关联金额/数量 ≤ 该订单未开票额度（发货 - 已开票）；
// 超限 → domain.ErrConservation；重复追加 → created=false 幂等（主键防重）。
func (r *Repo) LinkInvoiceOrderTx(ctx context.Context, tx *Tx, tenantID, invoiceID, orderID string, amountCents, qty int64) (created bool, err error) {
	// 行锁订单（并发关联串行化；未开票额度在锁内计算）
	var one string
	if err := tx.tx.QueryRow(ctx,
		`SELECT id FROM orders WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, orderID).Scan(&one); err != nil {
		if err == pgx.ErrNoRows {
			return false, &ErrNotFound{What: "order " + orderID}
		}
		return false, err
	}
	// 已关联 → 幂等返回（先于守恒：重复追加不算超额）
	var existing int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT 1 FROM invoice_order_links WHERE tenant_id=$1 AND invoice_id=$2 AND order_id=$3 LIMIT 1`,
		tenantID, invoiceID, orderID).Scan(&existing); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var shippedCents, shippedQty, invoicedCents, invoicedQty int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.qty * oi.unit_price_cents), 0), COALESCE(SUM(si.qty), 0)
		FROM shipment_items si
		JOIN shipments s ON s.id = si.shipment_id AND s.tenant_id = $1
		JOIN order_items oi ON oi.id = si.order_item_id
		WHERE s.order_id = $2 AND s.state IN ('SHIPPED','CLOSED')`,
		tenantID, orderID).Scan(&shippedCents, &shippedQty); err != nil {
		return false, err
	}
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(link.amount_cents), 0), COALESCE(SUM(link.qty), 0)
		FROM invoice_order_links link
		JOIN invoices i ON i.id = link.invoice_id AND i.tenant_id = $1
		WHERE link.tenant_id = $1 AND link.order_id = $2`,
		tenantID, orderID).Scan(&invoicedCents, &invoicedQty); err != nil {
		return false, err
	}
	if err := domain.AssertInvoiceWithinShipped(qty, amountCents, shippedQty-invoicedQty, shippedCents-invoicedCents); err != nil {
		return false, err
	}
	ct, err := tx.tx.Exec(ctx, `
		INSERT INTO invoice_order_links (tenant_id, invoice_id, order_id, amount_cents, qty)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, invoice_id, order_id) DO NOTHING`,
		tenantID, invoiceID, orderID, amountCents, qty)
	if err != nil {
		return false, fmt.Errorf("insert invoice_order_link: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return false, nil // 幂等：已关联
	}
	// 更新发票三金额快照（发货额并入关联订单；开票额累加）
	if _, err := tx.tx.Exec(ctx, `
		UPDATE invoices SET shipped_cents = shipped_cents + $4, invoiced_cents = invoiced_cents + $5,
		                    updated_at = now()
		WHERE tenant_id=$1 AND id=$2 AND order_id<>$3`,
		tenantID, invoiceID, orderID, shippedCents, amountCents); err != nil {
		return false, err
	}
	return true, nil
}

// LinkInvoiceOrder 追加关联（自开事务封装；API 使用）。
func (r *Repo) LinkInvoiceOrder(ctx context.Context, tenantID, invoiceID, orderID string, amountCents, qty int64) (bool, error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	created, err := r.LinkInvoiceOrderTx(ctx, tx, tenantID, invoiceID, orderID, amountCents, qty)
	if err != nil {
		return false, err
	}
	return created, tx.Commit(ctx)
}

// ListInvoiceLinks 发票全部关联订单（A4：三金额分展的数据源）。
func (r *Repo) ListInvoiceLinks(ctx context.Context, tenantID, invoiceID string) ([]InvoiceOrderLink, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT tenant_id, invoice_id, order_id, amount_cents, qty, linked_at
		FROM invoice_order_links WHERE tenant_id=$1 AND invoice_id=$2 ORDER BY linked_at, order_id`,
		tenantID, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InvoiceOrderLink
	for rows.Next() {
		var l InvoiceOrderLink
		if err := rows.Scan(&l.TenantID, &l.InvoiceID, &l.OrderID, &l.AmountCents, &l.Qty, &l.LinkedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AppendGatekeeperConfirmation 守门员确认事件（A3/N-01；append-only 审计）。
// 幂等：同一订单同一内容指纹（detail.content_hash）只记录一次；
// already=true 时返回既有事件 ID，调用方不再重复发送。
func (r *Repo) AppendGatekeeperConfirmation(ctx context.Context, tenantID, orderID, contentHash string, detail []byte, operator string) (eventID int64, already bool, err error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	var existing int64
	err = tx.tx.QueryRow(ctx, `
		SELECT id FROM order_events
		WHERE tenant_id=$1 AND order_id=$2 AND action='GatekeeperConfirmed'
		  AND detail->>'content_hash' = $3
		ORDER BY id LIMIT 1`,
		tenantID, orderID, contentHash).Scan(&existing)
	switch {
	case err == nil:
		return existing, true, tx.Commit(ctx)
	case err != pgx.ErrNoRows:
		return 0, false, err
	}
	var op *string
	if operator != "" {
		op = &operator
	}
	err = tx.tx.QueryRow(ctx, `
		INSERT INTO order_events (tenant_id, order_id, stage_code, task_id, action, from_state, to_state, reason, detail, error, operator_id)
		VALUES ($1,$2,'SHP',NULL,'GatekeeperConfirmed',NULL,NULL,'发群守门员确认', $3, NULL, $4)
		RETURNING id`,
		tenantID, orderID, detail, op).Scan(&eventID)
	if err != nil {
		return 0, false, err
	}
	return eventID, false, tx.Commit(ctx)
}

// LookupWarehouseGroupBinding 仓库群绑定（A6：仓库群 → 租户 + 仓库；凭证机器人识别回单归属）。
// binding JSON：{"kind":"warehouse_group","warehouse_id":"wh-..."}；非仓库群 → ErrNotFound。
func (r *Repo) LookupWarehouseGroupBinding(ctx context.Context, groupID string) (tenantID, warehouseID string, err error) {
	var binding []byte
	err = r.pool.QueryRow(ctx, `
		SELECT tenant_id, binding FROM wecom_context_bindings WHERE group_id=$1 LIMIT 1`, groupID).
		Scan(&tenantID, &binding)
	if err == pgx.ErrNoRows {
		return "", "", &ErrNotFound{What: "wecom binding for group " + groupID}
	}
	if err != nil {
		return "", "", err
	}
	var b struct {
		Kind        string `json:"kind"`
		WarehouseID string `json:"warehouse_id"`
	}
	if err := jsonUnmarshal(binding, &b); err != nil {
		return "", "", fmt.Errorf("parse warehouse group binding: %w", err)
	}
	if b.Kind != "warehouse_group" || b.WarehouseID == "" {
		return "", "", &ErrNotFound{What: "warehouse_group binding for group " + groupID}
	}
	return tenantID, b.WarehouseID, nil
}

// TrackPaymentTx 回款登记（自事务；A4：更新该订单关联发票的收款金额快照；
// 权威口径为 PaymentTracked 事件聚合）。合并开票下订单可能非发票主订单——经 links 关联。
func (r *Repo) TrackPaymentTx(ctx context.Context, tenantID, orderID string, amount int64) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.tx.Exec(ctx, `
		UPDATE invoices i SET received_cents = received_cents + $3, updated_at = now()
		WHERE i.tenant_id = $1 AND (i.order_id = $2 OR EXISTS (
			SELECT 1 FROM invoice_order_links l
			WHERE l.tenant_id = $1 AND l.invoice_id = i.id AND l.order_id = $2))`,
		tenantID, orderID, amount)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InvoiceNoExists INV-01：发票号是否已在任意发货单凭证上登记（重复发票硬校验）。
func (r *Repo) InvoiceNoExists(ctx context.Context, tenantID, invoiceNo string) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM evidence_versions WHERE tenant_id=$1 AND invoice_no=$2 AND kind='INVOICE')`,
		tenantID, invoiceNo).Scan(&exists); err != nil {
		return false, fmt.Errorf("check invoice_no exists: %w", err)
	}
	return exists, nil
}
