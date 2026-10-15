-- 001_init.sql —— 核心骨架：租户/用户/客户/产品/仓库/库存/订单/明细/ORD 事件链/迁移映射
-- 设计来源：docs/02-architecture.html §3.1（多租户隔离总则：复合 FK/唯一键一律带 tenant_id）
-- 幂等：IF NOT EXISTS，可重跑（L1-06 迁移校验）

BEGIN;

CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    name          TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('SALES','SUPERVISOR','CHAIRMAN','OP','ACCOUNTING','WAREHOUSE','ADMIN')),
    wecom_user_id TEXT,
    UNIQUE (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS customers (
    id        TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    name      TEXT NOT NULL,
    grade     TEXT NOT NULL CHECK (grade IN ('A','B','C','D')),
    owner_id  TEXT NOT NULL,
    UNIQUE (tenant_id, id),
    -- 租户维度复合 FK（02 §3.1 多租户隔离总则：资源间 FK 必须带租户维度）
    FOREIGN KEY (tenant_id, owner_id) REFERENCES users(tenant_id, id)
);

CREATE TABLE IF NOT EXISTS products (
    id        TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    sku       TEXT NOT NULL,
    name      TEXT NOT NULL,
    UNIQUE (tenant_id, sku),
    UNIQUE (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS warehouses (
    id        TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    name      TEXT NOT NULL,
    UNIQUE (tenant_id, id)
);

-- 库存（CQ-08）：仓库 × 产品，数量守恒载体；防线 1：CHECK (qty >= 0)
CREATE TABLE IF NOT EXISTS inventory_items (
    tenant_id    TEXT NOT NULL,
    warehouse_id TEXT NOT NULL,
    product_id   TEXT NOT NULL,
    qty          BIGINT NOT NULL DEFAULT 0 CHECK (qty >= 0),
    version      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, warehouse_id, product_id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES warehouses(tenant_id, id),
    FOREIGN KEY (tenant_id, product_id)   REFERENCES products(tenant_id, id)
);

-- 订单主档（四分支状态机载体，CQ-04/05/11/12）
CREATE TABLE IF NOT EXISTS orders (
    id                      TEXT PRIMARY KEY,
    tenant_id               TEXT NOT NULL REFERENCES tenants(id),
    customer_id             TEXT NOT NULL,
    state                   TEXT NOT NULL,
    payment_term            TEXT NOT NULL CHECK (payment_term IN ('款到','非款到')),
    supplier_channel        TEXT NOT NULL DEFAULT '',
    contract_no             TEXT,
    idempotency_key         TEXT NOT NULL,
    payload_hash            TEXT NOT NULL,
    payload                 JSONB NOT NULL DEFAULT '{}',
    legacy_flow_id          TEXT,
    submission_mode         TEXT NOT NULL DEFAULT 'create' CHECK (submission_mode IN ('create','copy','resubmit')),
    resubmit_from_order_id  TEXT,
    resubmit_root_order_id  TEXT,
    resubmit_revision       INT NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, idempotency_key),          -- 防线 3：订单幂等
    UNIQUE (tenant_id, legacy_flow_id),           -- 迁移追溯：legacy flow 唯一
    FOREIGN KEY (tenant_id, customer_id) REFERENCES customers(tenant_id, id),
    FOREIGN KEY (tenant_id, resubmit_from_order_id) REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, resubmit_root_order_id) REFERENCES orders(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_orders_tenant_state ON orders (tenant_id, state);
CREATE INDEX IF NOT EXISTS idx_orders_tenant_customer ON orders (tenant_id, customer_id);

-- 订单明细（数量守恒基准，CQ-06/09；防线 1：quantity > 0）
CREATE TABLE IF NOT EXISTS order_items (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    order_id         TEXT NOT NULL,
    product_id       TEXT NOT NULL,
    warehouse_id     TEXT,
    quantity         BIGINT NOT NULL CHECK (quantity > 0),
    unit_price_cents BIGINT NOT NULL CHECK (unit_price_cents >= 0),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, order_id)   REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products(tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES warehouses(tenant_id, id)
);

-- ORD 事件链（append-only，防线 6；CQ-12）
CREATE TABLE IF NOT EXISTS order_events (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    order_id    TEXT NOT NULL,
    stage_code  TEXT NOT NULL,
    task_id     TEXT,
    action      TEXT NOT NULL,
    from_state  TEXT,
    to_state    TEXT,
    reason      TEXT,
    detail      JSONB,
    error       TEXT,
    operator_id TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_events_tenant_order ON order_events (tenant_id, order_id, id);

-- 迁移追溯映射（docs/02-architecture.html §8.2）
CREATE TABLE IF NOT EXISTS legacy_flow_mapping (
    tenant_id          TEXT NOT NULL,
    legacy_flow_id     TEXT NOT NULL,
    legacy_stage_id    TEXT,
    stage_code         TEXT NOT NULL,
    stage_seq          INT,
    legacy_instance_id TEXT NOT NULL,
    legacy_task_id     TEXT,
    target_type        TEXT,
    target_id          TEXT,
    target_order_id    TEXT,
    target_revision    INT NOT NULL DEFAULT 0,
    imported_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, stage_code, legacy_instance_id),
    FOREIGN KEY (tenant_id, target_order_id) REFERENCES orders(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_legacy_flow ON legacy_flow_mapping (tenant_id, legacy_flow_id);

-- 迁移批次幂等锚点（§8.3 幂等红线）
CREATE TABLE IF NOT EXISTS migration_batches (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    source      TEXT NOT NULL,
    checksum    TEXT NOT NULL,
    state       TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    error       TEXT,
    UNIQUE (tenant_id, source, checksum)
);

COMMIT;
