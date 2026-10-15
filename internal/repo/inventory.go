package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/domain"
)

// SetInventory 设置/覆盖库存数量（种子数据与测试注资；业务路径不直接调用）。
func (r *Repo) SetInventory(ctx context.Context, tenantID, warehouseID, productID string, qty int64) error {
	if _, err := domain.NewNonNegQty(qty); err != nil {
		return err
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO inventory_items (tenant_id, warehouse_id, product_id, qty, version)
		VALUES ($1,$2,$3,$4,1)
		ON CONFLICT (tenant_id, warehouse_id, product_id)
		DO UPDATE SET qty = EXCLUDED.qty, version = inventory_items.version + 1`,
		tenantID, warehouseID, productID, qty)
	return err
}

// GetInventory 查询库存。
func (r *Repo) GetInventory(ctx context.Context, tenantID, warehouseID, productID string) (*InventoryItem, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT tenant_id, warehouse_id, product_id, qty, version
		FROM inventory_items WHERE tenant_id=$1 AND warehouse_id=$2 AND product_id=$3`,
		tenantID, warehouseID, productID)
	var it InventoryItem
	err := row.Scan(&it.TenantID, &it.WarehouseID, &it.ProductID, &it.Qty, &it.Version)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "inventory " + tenantID + "/" + warehouseID + "/" + productID}
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// LockInventory 行锁读取库存（防线 2：并发扣减串行化）。
func (r *Repo) LockInventory(ctx context.Context, tx *Tx, tenantID, warehouseID, productID string) (*InventoryItem, error) {
	row := tx.tx.QueryRow(ctx, `
		SELECT tenant_id, warehouse_id, product_id, qty, version
		FROM inventory_items WHERE tenant_id=$1 AND warehouse_id=$2 AND product_id=$3 FOR UPDATE`,
		tenantID, warehouseID, productID)
	var it InventoryItem
	err := row.Scan(&it.TenantID, &it.WarehouseID, &it.ProductID, &it.Qty, &it.Version)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "inventory " + tenantID + "/" + warehouseID + "/" + productID}
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// DeductInventoryTx 事务内库存扣减（守恒断言 + CHECK 约束双保险；出与发货同事务，CQ-08）。
func (r *Repo) DeductInventoryTx(ctx context.Context, tx *Tx, tenantID, warehouseID, productID string, qty int64) error {
	inv, err := r.LockInventory(ctx, tx, tenantID, warehouseID, productID)
	if err != nil {
		return err
	}
	if err := domain.AssertInventoryNonNeg(inv.Qty, qty); err != nil {
		return err
	}
	_, err = tx.tx.Exec(ctx, `
		UPDATE inventory_items SET qty = qty - $4, version = version + 1
		WHERE tenant_id=$1 AND warehouse_id=$2 AND product_id=$3`,
		tenantID, warehouseID, productID, qty)
	if err != nil {
		return fmt.Errorf("deduct inventory: %w", err)
	}
	return nil
}
