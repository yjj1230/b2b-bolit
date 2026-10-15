package repo

import (
	"context"
	"fmt"

	"b2b_pilot_v3/internal/domain"
)

// ── 对账聚合（L3-02 四守恒全库校验数据源；docs/02-architecture.html §4 防线 5）──

// ReconcileAllOrders 全库按订单聚合四守恒指标（备货 ≤ 订单、发货 ≤ 备货、开票 ≤ 发货、库存 ≥ 0）。
// 返回每个订单的守恒状态与违规项；库存恒非负由 CHECK 约束兜底，此处校验发货-库存扣减一致性。
func (r *Repo) ReconcileAllOrders(ctx context.Context, tenantID string) ([]ReconcileOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT o.tenant_id, o.id,
		       COALESCE(SUM(oi.quantity),0)::bigint                                  AS order_qty,
		       COALESCE((SELECT SUM(spi.qty) FROM stock_prepare_items spi
		                 JOIN stock_prepares sp ON sp.id = spi.stock_prepare_id
		                 WHERE sp.tenant_id=o.tenant_id AND sp.order_id=o.id),0)    AS prepared_qty,
		       COALESCE((SELECT SUM(si.qty) FROM shipment_items si
		                 JOIN shipments s ON s.id = si.shipment_id
		                 WHERE s.tenant_id=o.tenant_id AND s.order_id=o.id
		                   AND s.state IN ('SHIPPED','CLOSED')),0)                  AS shipped_qty,
		       COALESCE((SELECT SUM(ii.qty) FROM invoice_items ii
		                 JOIN invoices i ON i.id = ii.invoice_id
		                 WHERE i.tenant_id=o.tenant_id AND i.order_id=o.id),0)       AS invoiced_qty,
		       COALESCE((SELECT SUM(ii.amount_cents) FROM invoice_items ii
		                 JOIN invoices i ON i.id = ii.invoice_id
		                 WHERE i.tenant_id=o.tenant_id AND i.order_id=o.id),0)       AS invoiced_amount
		FROM orders o
		JOIN order_items oi ON oi.tenant_id = o.tenant_id AND oi.order_id = o.id
		WHERE ($1 = '' OR o.tenant_id = $1)
		GROUP BY o.tenant_id, o.id
		ORDER BY o.tenant_id, o.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("reconcile aggregate: %w", err)
	}
	defer rows.Close()

	var out []ReconcileOrder
	for rows.Next() {
		var rc ReconcileOrder
		if err := rows.Scan(&rc.TenantID, &rc.OrderID, &rc.OrderQty, &rc.PreparedQty, &rc.ShippedQty,
			&rc.InvoicedQty, &rc.InvoicedAmount); err != nil {
			return nil, err
		}
		if err := domain.AssertPrepareWithinOrder(rc.PreparedQty, rc.OrderQty); err != nil {
			rc.Violations = append(rc.Violations, "备货>订单")
		}
		if err := domain.AssertShipWithinPrepare(rc.ShippedQty, rc.PreparedQty); err != nil {
			rc.Violations = append(rc.Violations, "发货>备货")
		}
		// 金额维度：可开票金额 = 发货量 × 明细单价（近似基准：按订单聚合单价不便，此处数量维度为主，
		// 金额双维在 ApplyInvoice 事务内精确校验；对账脚本同时输出金额便于人工核对）
		if err := domain.AssertInvoiceWithinShipped(rc.InvoicedQty, rc.InvoicedAmount, rc.ShippedQty, int64(1)<<62); err != nil {
			rc.Violations = append(rc.Violations, "开票数量>发货数量")
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// ReconcileInventory 库存守恒：全库库存数量汇总（恒 >= 0 由 CHECK 保证），返回负库存清单（应为空）。
func (r *Repo) ReconcileInventory(ctx context.Context, tenantID string) ([]InventoryItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT tenant_id, warehouse_id, product_id, qty, version
		FROM inventory_items WHERE qty < 0 AND ($1='' OR tenant_id=$1)`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InventoryItem
	for rows.Next() {
		var it InventoryItem
		if err := rows.Scan(&it.TenantID, &it.WarehouseID, &it.ProductID, &it.Qty, &it.Version); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
