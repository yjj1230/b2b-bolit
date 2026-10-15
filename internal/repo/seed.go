package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ── 基础数据播种（测试/种子脚本共用；确定性 ID）──

// SeedTenant 创建租户。
func (r *Repo) SeedTenant(ctx context.Context, id, name string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, id, name)
	return err
}

// SeedUser 创建用户。
func (r *Repo) SeedUser(ctx context.Context, tenantID, id, name, role, wecomUserID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO users (id, tenant_id, name, role, wecom_user_id) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name=EXCLUDED.name, role=EXCLUDED.role, wecom_user_id=EXCLUDED.wecom_user_id`,
		id, tenantID, name, role, wecomUserID)
	return err
}

// SeedCustomer 创建客户（D-006 后仅 legacy/测试回填使用；业务主数据经 pgAPI）。
//
// Deprecated: 新订单的 customer_id 是 pgAPI record_id，不再由本地 customers 表提供约束。
func (r *Repo) SeedCustomer(ctx context.Context, tenantID, id, name, grade, ownerID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO customers (id, tenant_id, name, grade, owner_id) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name=EXCLUDED.name, grade=EXCLUDED.grade, owner_id=EXCLUDED.owner_id`,
		id, tenantID, name, grade, ownerID)
	return err
}

// SeedProduct 创建产品（D-006 后仅 legacy/测试回填使用；业务主数据经 pgAPI）。
//
// Deprecated: 订单明细的 product_id 是 pgAPI record_id，不再由本地 products 表提供约束。
func (r *Repo) SeedProduct(ctx context.Context, tenantID, id, sku, name string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO products (id, tenant_id, sku, name) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, sku) DO UPDATE SET name=EXCLUDED.name`,
		id, tenantID, sku, name)
	return err
}

// SeedWarehouse 创建仓库。
func (r *Repo) SeedWarehouse(ctx context.Context, tenantID, id, name string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO warehouses (id, tenant_id, name) VALUES ($1,$2,$3)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name=EXCLUDED.name`,
		id, tenantID, name)
	return err
}

// SeedInventory 创建库存（不存在则插入；存在则保留原值，避免重复播种累加）。
func (r *Repo) SeedInventory(ctx context.Context, tenantID, warehouseID, productID string, qty int64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO inventory_items (tenant_id, warehouse_id, product_id, qty, version)
		VALUES ($1,$2,$3,$4,1)
		ON CONFLICT (tenant_id, warehouse_id, product_id) DO NOTHING`,
		tenantID, warehouseID, productID, qty)
	return err
}

// SeedWecomBinding 创建企微上下文绑定（07 §4）。
func (r *Repo) SeedWecomBinding(ctx context.Context, tenantID, externalUserID, groupID string, binding []byte) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO wecom_context_bindings (tenant_id, external_user_id, group_id, binding) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, external_user_id, group_id) DO UPDATE SET binding=EXCLUDED.binding`,
		tenantID, externalUserID, groupID, binding)
	return err
}

// LookupBinding 按群查租户绑定（消息 → 租户解析）。
func (r *Repo) LookupBinding(ctx context.Context, groupID string) (tenantID string, binding []byte, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT tenant_id, binding FROM wecom_context_bindings WHERE group_id=$1 LIMIT 1`, groupID).
		Scan(&tenantID, &binding)
	if err == pgx.ErrNoRows {
		return "", nil, &ErrNotFound{What: "wecom binding for group " + groupID}
	}
	return tenantID, binding, err
}

// SeedDefaultWorld 播种标准演示世界（长安仁恒）：租户 + 角色用户 + 四仓 + 库存 + 企微绑定。
// customers/products 不再本地播种（订单数据实体经 pgAPI，见 orderdata 包）。
// runID 用于区分多次播种（幂等：重复执行不报错）。
func (r *Repo) SeedDefaultWorld(ctx context.Context, runID string) error {
	const tenant = "chang-an"
	if err := r.SeedTenant(ctx, tenant, "长安仁恒"); err != nil {
		return err
	}
	users := []struct{ id, name, role, wc string }{
		{"u-sales", "销售-张三", "SALES", "wecom_sales"},
		{"u-supervisor", "主管-李四", "SUPERVISOR", "wecom_supervisor"},
		{"u-chairman", "董事长-王五", "CHAIRMAN", "wecom_chairman"},
		{"u-op", "商务-赵六", "OP", "wecom_op"},
		{"u-accounting", "财务-钱七", "ACCOUNTING", "wecom_accounting"},
		{"u-wh-yangyuan", "仓管-阳原", "WAREHOUSE", "wecom_wh_yangyuan"},
		{"u-wh-dongguan", "仓管-东莞", "WAREHOUSE", "wecom_wh_dongguan"},
		{"u-admin", "管理员", "ADMIN", "wecom_admin"},
	}
	for _, u := range users {
		if err := r.SeedUser(ctx, tenant, u.id, u.name, u.role, u.wc); err != nil {
			return err
		}
	}
	// customers/products 不再播种（D-006：订单数据实体经 orderdata/pgAPI；
	// 本地两表已由 migration 007 降级为 legacy，只保留旧数据与一次性映射回填）。
	warehouses := []struct{ id, name string }{
		{"wh-yangyuan", "阳原仓"},
		{"wh-changxing", "长兴仓"},
		{"wh-dongguan", "东莞仓"},
		{"wh-chengdu", "成都仓"},
	}
	for _, w := range warehouses {
		if err := r.SeedWarehouse(ctx, tenant, w.id, w.name); err != nil {
			return err
		}
	}
	for _, w := range []string{"wh-yangyuan", "wh-changxing", "wh-dongguan", "wh-chengdu"} {
		for _, p := range []string{"prod-c68", "prod-d90"} {
			if err := r.SeedInventory(ctx, tenant, w, p, 1000); err != nil {
				return err
			}
		}
	}
	if err := r.SeedWecomBinding(ctx, tenant, "ext-sales", "group-demo", []byte(fmt.Sprintf(`{"run_id":%q,"owner":"u-sales"}`, runID))); err != nil {
		return err
	}
	// 0.1 A6：仓库群绑定（阳原外租仓 → 凭证机器人回单归属）
	if err := r.SeedWecomBinding(ctx, tenant, "", "group-wh-yangyuan", []byte(`{"kind":"warehouse_group","warehouse_id":"wh-yangyuan"}`)); err != nil {
		return err
	}
	return nil
}
