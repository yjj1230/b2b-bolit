package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// ── 备货（STK）──

// CreateStockPrepare 幂等创建备货审批聚合（防线 3；备货守恒由调用方在事务内校验）。
func (r *Repo) CreateStockPrepare(ctx context.Context, tx *Tx, sp StockPrepare, items []StockPrepareItem) error {
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO stock_prepares (id, tenant_id, order_id, warehouse_id, state, planned_qty, remaining_qty,
		                            batch_seq, split_flag, previous_stock_prepare_id, outbound_order_ref, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE
			SET warehouse_id = EXCLUDED.warehouse_id, state = EXCLUDED.state`,
		sp.ID, sp.TenantID, sp.OrderID, sp.WarehouseID, sp.State, sp.PlannedQty, sp.RemainingQty,
		sp.BatchSeq, sp.SplitFlag, sp.PreviousStockPrepareID, sp.OutboundOrderRef, sp.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("insert stock_prepare: %w", err)
	}
	for _, it := range items {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO stock_prepare_items (id, tenant_id, stock_prepare_id, order_item_id, product_id, qty)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (tenant_id, id) DO NOTHING`,
			it.ID, it.TenantID, it.StockPrepareID, it.OrderItemID, it.ProductID, it.Qty); err != nil {
			return fmt.Errorf("insert stock_prepare_item: %w", err)
		}
	}
	return nil
}

// ── 发货（SHP）：单事务：发货单 + 明细 + 库存扣减 + 事件（防线 4；CQ-07/08）──

// ApplyShipment 一次实际发货。校验链（事务内）：
//  1. 行锁发货单/备货单（并发串行化）
//  2. 发货守恒：∑(本次发货) ≤ 备货确认量（按 order_item）
//  3. 库存守恒：∑(本次发货) ≤ 库存（每 product 行锁）
//  4. 发货 + 扣库存 + shipment_event 同事务提交
func (r *Repo) ApplyShipment(ctx context.Context, tx *Tx, ship Shipment, items []ShipmentItem, operator string) error {
	// 行锁备货单（串行化同一备货单的并发发货）
	if err := tx.tx.QueryRow(ctx,
		`SELECT id FROM stock_prepares WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
		ship.TenantID, ship.StockPrepareID).Scan(&ship.StockPrepareID); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: "stock_prepare " + ship.StockPrepareID}
		}
		return err
	}

	// 备货守恒：本次发货量 ≤ 备货确认量（按 order_item）
	for _, it := range items {
		var planned int64
		if err := tx.tx.QueryRow(ctx, `
			SELECT qty FROM stock_prepare_items
			WHERE tenant_id=$1 AND stock_prepare_id=$2 AND order_item_id=$3`,
			ship.TenantID, ship.StockPrepareID, it.OrderItemID).Scan(&planned); err != nil {
			if err == pgx.ErrNoRows {
				return &ErrNotFound{What: fmt.Sprintf("stock_prepare_item %s/%s", ship.StockPrepareID, it.OrderItemID)}
			}
			return err
		}
		// 累计已发（同备货单） + 本次
		var already int64
		if err := tx.tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(si.qty),0) FROM shipment_items si
			JOIN shipments s ON s.id = si.shipment_id
			WHERE s.tenant_id=$1 AND s.stock_prepare_id=$2 AND si.order_item_id=$3 AND s.state IN ('SHIPPED','CLOSED')`,
			ship.TenantID, ship.StockPrepareID, it.OrderItemID).Scan(&already); err != nil {
			return err
		}
		if err := domain.AssertShipWithinPrepare(already+it.Qty, planned); err != nil {
			return err
		}
	}

	// 插入发货单 + 明细（幂等：同键重放 → 跳过，防线 3/7）
	ct, err := tx.tx.Exec(ctx, `
		INSERT INTO shipments (id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty,
		                       shipped_qty, batch_seq, split_flag, previous_shipment_id, remaining_quantity, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		ship.ID, ship.TenantID, ship.OrderID, ship.StockPrepareID, ship.WarehouseID, domain.ShipmentStateShipped,
		ship.PlannedQty, ship.ShippedQty, ship.BatchSeq, ship.SplitFlag, ship.PreviousShipmentID,
		ship.RemainingQuantity, ship.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("insert shipment: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// 幂等重放：同键发货已存在 → 视为已应用
		return nil
	}
	for _, it := range items {
		if _, err := tx.tx.Exec(ctx, `
			INSERT INTO shipment_items (id, tenant_id, shipment_id, order_item_id, product_id, qty)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			it.ID, it.TenantID, it.ShipmentID, it.OrderItemID, it.ProductID, it.Qty); err != nil {
			return fmt.Errorf("insert shipment_item: %w", err)
		}
		// M1（0.1 暂缓）：锁库存不做——发货不扣库存（docs/v0.1/04-architecture §5）；
		// 发货守恒校验照常（∑发货 ≤ ∑备货，防线 5）。
	}
	// 事件链（append-only）
	if err := r.appendShipmentEvent(ctx, tx, ship.TenantID, ship.ID, ship.StockPrepareID, ship.OrderID,
		"SHP", nil, "GoodsShipped", "", domain.ShipmentStateShipped, "实际发货（0.1 锁库存暂缓：库存不变）", nil, nil, operator); err != nil {
		return err
	}
	return nil
}

// CloseShipment 关闭发货单（一张发货单只发一次；SHIPPED → CLOSED）。
func (r *Repo) CloseShipment(ctx context.Context, tx *Tx, tenantID, shipmentID, operator string) error {
	if err := tx.tx.QueryRow(ctx,
		`UPDATE shipments SET state='CLOSED', updated_at=now() WHERE tenant_id=$1 AND id=$2 AND state='SHIPPED' RETURNING id`,
		tenantID, shipmentID).Scan(&shipmentID); err != nil {
		if err == pgx.ErrNoRows {
			return &ErrNotFound{What: "shipment " + shipmentID + " in SHIPPED"}
		}
		return err
	}
	return r.appendShipmentEvent(ctx, tx, tenantID, shipmentID, "", "", "SHP", nil, "ShipmentClosed",
		domain.ShipmentStateShipped, domain.ShipmentStateClosed, "发货单关闭", nil, nil, operator)
}

// GetStockPrepare 查询备货单（0.1 选仓生效：发货单继承所选仓库）。
func (r *Repo) GetStockPrepare(ctx context.Context, tenantID, stockPrepareID string) (*StockPrepare, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, order_id, warehouse_id, state, planned_qty, remaining_qty,
		       batch_seq, split_flag, previous_stock_prepare_id, outbound_order_ref, idempotency_key, created_at
		FROM stock_prepares WHERE tenant_id=$1 AND id=$2`, tenantID, stockPrepareID)
	var s StockPrepare
	err := row.Scan(&s.ID, &s.TenantID, &s.OrderID, &s.WarehouseID, &s.State, &s.PlannedQty, &s.RemainingQty,
		&s.BatchSeq, &s.SplitFlag, &s.PreviousStockPrepareID, &s.OutboundOrderRef, &s.IdempotencyKey, &s.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "stock_prepare " + stockPrepareID}
	}
	if err != nil {
		return nil, fmt.Errorf("scan stock_prepare: %w", err)
	}
	return &s, nil
}

// GetShipment 查询发货单。
func (r *Repo) GetShipment(ctx context.Context, tenantID, shipmentID string) (*Shipment, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, shipped_qty,
		       batch_seq, split_flag, previous_shipment_id, remaining_quantity, idempotency_key, created_at, updated_at
		FROM shipments WHERE tenant_id=$1 AND id=$2`, tenantID, shipmentID)
	var s Shipment
	err := row.Scan(&s.ID, &s.TenantID, &s.OrderID, &s.StockPrepareID, &s.WarehouseID, &s.State,
		&s.PlannedQty, &s.ShippedQty, &s.BatchSeq, &s.SplitFlag, &s.PreviousShipmentID,
		&s.RemainingQuantity, &s.IdempotencyKey, &s.CreatedAt, &s.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "shipment " + shipmentID}
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SumShippedByOrderItemTx 按订单明细聚合有效发货量（事务内，开票守恒基准，CQ-09）。
func (r *Repo) SumShippedByOrderItemTx(ctx context.Context, tx *Tx, tenantID, orderID string) (map[string]int64, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT si.order_item_id, SUM(si.qty) FROM shipment_items si
		JOIN shipments s ON s.id = si.shipment_id
		WHERE s.tenant_id=$1 AND s.order_id=$2 AND s.state IN ('SHIPPED','CLOSED')
		GROUP BY si.order_item_id`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (r *Repo) appendShipmentEvent(ctx context.Context, tx *Tx, tenantID, shipmentID, stockPrepareID, orderID, stage string, taskID *string, action, fromState, toState, reason string, detail []byte, errStr *string, operator string) error {
	var errS, op *string
	if errStr != nil {
		errS = errStr
	}
	if operator != "" {
		op = &operator
	}
	var sp *string
	if stockPrepareID != "" {
		sp = &stockPrepareID
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO shipment_events (tenant_id, shipment_id, stock_prepare_id, order_id, stage_code, task_id,
		                             action, from_state, to_state, reason, detail, error, operator_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		tenantID, shipmentID, sp, orderID, stage, taskID, action, fromState, toState, reason, detail, errS, op)
	return err
}

// ListShipmentsByOrder UI-02：订单发货单列表（商务操作台凭证登记入口）。
func (r *Repo) ListShipmentsByOrder(ctx context.Context, tenantID, orderID string) ([]Shipment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, shipped_qty,
		       batch_seq, split_flag, previous_shipment_id, remaining_quantity, idempotency_key, created_at, updated_at
		FROM shipments WHERE tenant_id=$1 AND order_id=$2 ORDER BY created_at`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Shipment{}
	for rows.Next() {
		var s Shipment
		if err := rows.Scan(&s.ID, &s.TenantID, &s.OrderID, &s.StockPrepareID, &s.WarehouseID, &s.State,
			&s.PlannedQty, &s.ShippedQty, &s.BatchSeq, &s.SplitFlag, &s.PreviousShipmentID,
			&s.RemainingQuantity, &s.IdempotencyKey, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
