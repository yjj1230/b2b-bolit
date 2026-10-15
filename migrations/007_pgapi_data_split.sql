-- 007_pgapi_data_split.sql —— 数据库访问二分：流程引擎 PG vs 订单数据实体 pgAPI
-- 决策来源：project/decisions/D-006.yaml（Q2=A）
--   1) orders.customer_id 与 order_items/stock_prepare_items/shipment_items/invoice_items/
--      inventory_items.product_id 改为 pgAPI record_id 外部引用，去除本地 FK；
--   2) customers/products 两张本地表降级为 legacy 只读归档（应用不再读写）；
--   3) 新增 pgapi_entity_mappings 一次性映射表，供回填 old_id -> record_id 与审计。
-- 幂等：IF EXISTS / IF NOT EXISTS，可重复执行。

BEGIN;

-- ── 去除所有指向本地 customers/products 的外键（不依赖默认命名）──
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT conrelid::regclass::text AS tbl, conname
        FROM pg_constraint
        WHERE contype = 'f'
          AND confrelid IN ('customers'::regclass, 'products'::regclass)
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.conname);
    END LOOP;
END $$;

-- ── 本地主数据表降级为 legacy（保留数据，不参与业务访问）──
COMMENT ON TABLE customers IS 'legacy：D-006 后订单数据实体经 pgAPI；本表仅归档/回填对账，应用零访问';
COMMENT ON TABLE products  IS 'legacy：D-006 后订单数据实体经 pgAPI；本表仅归档/回填对账，应用零访问';

-- ── 一次性 ID 映射：old_id -> pgAPI record_id（回填审计用）──
CREATE TABLE IF NOT EXISTS pgapi_entity_mappings (
    tenant_id   TEXT NOT NULL,
    entity_type TEXT NOT NULL CHECK (entity_type IN ('customer','product')),
    legacy_id   TEXT NOT NULL,
    record_id   TEXT NOT NULL,
    matched_by  TEXT NOT NULL,
    matched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, entity_type, legacy_id)
);
CREATE INDEX IF NOT EXISTS idx_pgapi_entity_mappings_record
    ON pgapi_entity_mappings (tenant_id, entity_type, record_id);

COMMENT ON TABLE pgapi_entity_mappings IS
    'D-006：本地 cust-*/prod-* 与 pgAPI record_id 的一次性映射审计表；回填完成后只读';

COMMIT;
