package repo

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ── 自事务包装（workflows.Store 接口实现；每个方法一个原子事务）──
// 设计：docs/02-architecture.html §4 防线 4（发货+库存扣减 / 开票+守恒 / 状态+事件链 各自同事务）。

// ShipAndCloseShipmentTx 发货 + 订单状态迁移（两个原子单元：
// ① 发货+库存扣减+shipment_event；② order 状态+order_event）。
func (r *Repo) ShipAndUpdateOrderTx(ctx context.Context, ship Shipment, items []ShipmentItem,
	fromState, toState, operator, reason string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.ApplyShipment(ctx, tx, ship, items, operator); err != nil {
		return err
	}
	if err := r.UpdateOrderState(ctx, tx, ship.TenantID, ship.OrderID, fromState, toState,
		"GoodsShipped", operator, reason, detail); err != nil {
		return err
	}
	// Outbox 投影事件（决策 D4）：发货单创建 → shipment#requester + shipment#warehouse_operator（场景 B/C）
	if err := r.AppendOutboxEvent(ctx, tx, ship.TenantID, "shipment", ship.ID, "shipment.created",
		MarshalJSONDetail(map[string]any{"requester_id": "u-op", "warehouse_operator_id": operator})); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InvoiceAndUpdateOrderTx 开票 + 订单状态迁移。
func (r *Repo) InvoiceAndUpdateOrderTx(ctx context.Context, inv Invoice, items []InvoiceItem,
	fromState, toState, operator, reason string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.ApplyInvoice(ctx, tx, inv, items, operator); err != nil {
		return err
	}
	if err := r.UpdateOrderState(ctx, tx, inv.TenantID, inv.OrderID, fromState, toState,
		"InvoiceIssued", operator, reason, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReserveStockTx 备货审批聚合 + 订单状态迁移。
func (r *Repo) ReserveStockTx(ctx context.Context, sp StockPrepare, items []StockPrepareItem,
	fromState, toState, operator, reason string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.CreateStockPrepare(ctx, tx, sp, items); err != nil {
		return err
	}
	if err := r.UpdateOrderState(ctx, tx, sp.TenantID, sp.OrderID, fromState, toState,
		"StockAssigned", operator, reason, detail); err != nil {
		return err
	}
	// Outbox 投影事件（决策 D4）：备货单创建 → prestock#owner（场景 A/B）
	if err := r.AppendOutboxEvent(ctx, tx, sp.TenantID, "prestock", sp.ID, "prestock.created",
		MarshalJSONDetail(map[string]any{"owner_id": "u-op"})); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateOrderStateTx 订单状态迁移（自事务）。
func (r *Repo) UpdateOrderStateTx(ctx context.Context, tenantID, orderID, fromState, toState, action, operator, reason string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.UpdateOrderState(ctx, tx, tenantID, orderID, fromState, toState, action, operator, reason, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateApprovalTaskTx 审批任务创建（自事务）。
func (r *Repo) CreateApprovalTaskTx(ctx context.Context, in NewApprovalTask) (string, error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	id, err := r.CreateApprovalTask(ctx, tx, in)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// DecideTaskTx 审批裁决（自事务）。
func (r *Repo) DecideTaskTx(ctx context.Context, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.DecideTask(ctx, tx, tenantID, taskID, toState, verdict, decidedBy, detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SumShippedByOrderItem 按订单明细聚合有效发货量（只读；Store 契约，CQ-07/09）。
func (r *Repo) SumShippedByOrderItem(ctx context.Context, tenantID, orderID string) (map[string]int64, error) {
	rows, err := r.pool.Query(ctx, `
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

// CountShipments 统计订单发货单数量（部分发货序号派生）。
func (r *Repo) CountShipments(ctx context.Context, tenantID, orderID string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM shipments WHERE tenant_id=$1 AND order_id=$2`, tenantID, orderID).Scan(&n)
	return n, err
}

// UpdateResubmitChainTx 修改产生的 resubmit 链：revision 递增 + from 链（自事务）。
func (r *Repo) UpdateResubmitChainTx(ctx context.Context, tenantID, orderID string, revision int, fromOrderID string) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.tx.Exec(ctx, `
		UPDATE orders SET resubmit_revision=$3, resubmit_from_order_id=$4, submission_mode='resubmit', updated_at=now()
		WHERE tenant_id=$1 AND id=$2`,
		tenantID, orderID, revision, fromOrderID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordFlowFailed 补偿事件留痕（不改状态，append-only；CQ-12）。
func (r *Repo) RecordFlowFailed(ctx context.Context, tenantID, orderID, reason string, detail []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.appendOrderEvent(ctx, tx, tenantID, orderID, "SYS", nil, "FlowFailed",
		"", "", reason, detail, nil, "system"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetCustomerGrade 查询客户等级（D-006 后仅 legacy/测试兼容）。
//
// Deprecated: 业务流程经 internal/orderdata → pgAPI；本方法不再被 workflows.Store 引用。
func (r *Repo) GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error) {
	var grade string
	err := r.pool.QueryRow(ctx, `SELECT grade FROM customers WHERE tenant_id=$1 AND id=$2`, tenantID, customerID).Scan(&grade)
	if err == pgx.ErrNoRows {
		return "", &ErrNotFound{What: "customer " + customerID}
	}
	if err != nil {
		return "", err
	}
	return grade, nil
}

// UpdateOrderMetaTx 更新订单元信息（合同号/payload），不触发状态迁移（E 类静默修改）。
func (r *Repo) UpdateOrderMetaTx(ctx context.Context, tenantID, orderID, contractNo string, payload []byte) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.UpdateOrderMeta(ctx, tx, tenantID, orderID, contractNo, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetOrderContractTx 合同生成成功写入合同号（自事务）。
func (r *Repo) SetOrderContractTx(ctx context.Context, tenantID, orderID, contractNo string) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.SetOrderContract(ctx, tx, tenantID, orderID, contractNo); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdatePreOrderStateTx 预订单状态迁移（自事务）。
func (r *Repo) UpdatePreOrderStateTx(ctx context.Context, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error {
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.UpdatePreOrderState(ctx, tx, tenantID, preOrderID, fromState, toState, action, reason, detail, operator); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateOrderFromPreOrderTx 预订单确认派生正式订单（预订单状态 + 订单创建分属两事务，
// 靠 idempotency_key 串联——预订单事件与订单事件分属两表，设计如此）。
func (r *Repo) CreateOrderFromPreOrderTx(ctx context.Context, in NewOrder) (*Order, bool, error) {
	return r.CreateOrder(ctx, in)
}
