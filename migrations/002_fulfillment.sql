-- 002_fulfillment.sql —— 履约层：备货/发货/开票/审批任务/评论 + STK/SHP/INV 分域事件链
-- 设计来源：docs/02-architecture.html §3.1；四道防线（CHECK/行锁/UNIQUE 幂等/单事务守恒）

BEGIN;

-- 备货审批聚合（CQ-05/06/12；防线 1：planned_qty > 0、remaining_qty >= 0；防线 3：幂等唯一）
CREATE TABLE IF NOT EXISTS stock_prepares (
    id                      TEXT PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    order_id                TEXT NOT NULL,
    warehouse_id            TEXT NOT NULL,
    state                   TEXT NOT NULL,
    planned_qty             BIGINT NOT NULL CHECK (planned_qty > 0),
    remaining_qty           BIGINT NOT NULL CHECK (remaining_qty >= 0),
    batch_seq               INT NOT NULL DEFAULT 1,
    split_flag              BOOLEAN NOT NULL DEFAULT FALSE,
    previous_stock_prepare_id TEXT,
    outbound_order_ref      TEXT,
    idempotency_key         TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, idempotency_key),
    FOREIGN KEY (tenant_id, order_id)     REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES warehouses(tenant_id, id),
    FOREIGN KEY (tenant_id, previous_stock_prepare_id) REFERENCES stock_prepares(tenant_id, id)
);

CREATE TABLE IF NOT EXISTS stock_prepare_items (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    stock_prepare_id TEXT NOT NULL,
    order_item_id    TEXT NOT NULL,
    product_id       TEXT NOT NULL,
    qty              BIGINT NOT NULL CHECK (qty > 0),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, stock_prepare_id, order_item_id),
    FOREIGN KEY (tenant_id, stock_prepare_id) REFERENCES stock_prepares(tenant_id, id),
    FOREIGN KEY (tenant_id, order_item_id)    REFERENCES order_items(tenant_id, id),
    FOREIGN KEY (tenant_id, product_id)       REFERENCES products(tenant_id, id)
);

-- 发货单（CQ-07/12；防线 1：shipped_qty <= planned_qty；一张发货单只发一次）
CREATE TABLE IF NOT EXISTS shipments (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    order_id            TEXT NOT NULL,
    stock_prepare_id    TEXT NOT NULL,
    warehouse_id        TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('OPEN','SHIPPED','CLOSED')),
    planned_qty         BIGINT NOT NULL CHECK (planned_qty > 0),
    shipped_qty         BIGINT NOT NULL DEFAULT 0 CHECK (shipped_qty <= planned_qty),
    batch_seq           INT NOT NULL DEFAULT 1,
    split_flag          BOOLEAN NOT NULL DEFAULT FALSE,
    previous_shipment_id TEXT,
    remaining_quantity  BIGINT NOT NULL DEFAULT 0 CHECK (remaining_quantity >= 0),
    idempotency_key     TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, idempotency_key),
    FOREIGN KEY (tenant_id, order_id)         REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, stock_prepare_id) REFERENCES stock_prepares(tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id)     REFERENCES warehouses(tenant_id, id),
    FOREIGN KEY (tenant_id, previous_shipment_id) REFERENCES shipments(tenant_id, id)
);

CREATE TABLE IF NOT EXISTS shipment_items (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    shipment_id   TEXT NOT NULL,
    order_item_id TEXT NOT NULL,
    product_id    TEXT NOT NULL,
    qty           BIGINT NOT NULL CHECK (qty > 0),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, shipment_id)   REFERENCES shipments(tenant_id, id),
    FOREIGN KEY (tenant_id, order_item_id) REFERENCES order_items(tenant_id, id),
    FOREIGN KEY (tenant_id, product_id)    REFERENCES products(tenant_id, id)
);

-- 开票（CQ-09；防线 1：amount_cents >= 0；防线 3：幂等唯一）
CREATE TABLE IF NOT EXISTS invoices (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    order_id        TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('PENDING','PROCESSING','ISSUED','FAILED','NONE')),
    amount_cents    BIGINT NOT NULL CHECK (amount_cents >= 0),
    qty             BIGINT NOT NULL DEFAULT 0 CHECK (qty >= 0),
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, idempotency_key),
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders(tenant_id, id)
);

-- 开票明细（守恒校验基准：金额+数量双维度，防重复分配）
CREATE TABLE IF NOT EXISTS invoice_items (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    invoice_id       TEXT NOT NULL,
    order_item_id    TEXT NOT NULL,
    product_id       TEXT NOT NULL,
    qty              BIGINT NOT NULL CHECK (qty > 0),
    amount_cents     BIGINT NOT NULL CHECK (amount_cents > 0),
    idempotency_key  TEXT NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, invoice_id, order_item_id),
    FOREIGN KEY (tenant_id, invoice_id)    REFERENCES invoices(tenant_id, id),
    FOREIGN KEY (tenant_id, order_item_id) REFERENCES order_items(tenant_id, id),
    FOREIGN KEY (tenant_id, product_id)    REFERENCES products(tenant_id, id)
);

-- 审批任务（CQ-05/10；INV 七节点用 kind=INVOICE_* 承载，invoices.state 保持聚合态）
CREATE TABLE IF NOT EXISTS approval_tasks (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    order_id        TEXT NOT NULL,
    stage_code      TEXT NOT NULL CHECK (stage_code IN ('ORD','STK','SHP','INV')),
    kind            TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('PENDING','APPROVED','REJECTED','RETURNED','ESCALATED','CANCELLED')),
    assignee_id     TEXT,
    legacy_task_id  TEXT,
    deadline        TIMESTAMPTZ,
    verdict         TEXT,
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, assignee_id) REFERENCES users(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_approval_tenant_assignee ON approval_tasks (tenant_id, assignee_id, state);
CREATE INDEX IF NOT EXISTS idx_approval_tenant_kind ON approval_tasks (tenant_id, kind, state);

-- STK/SHP/INV 分域事件链（append-only，防线 6；CQ-12）
CREATE TABLE IF NOT EXISTS stock_prepare_events (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    stock_prepare_id TEXT NOT NULL,
    order_id         TEXT NOT NULL,
    stage_code       TEXT NOT NULL,
    task_id          TEXT,
    action           TEXT NOT NULL,
    from_state       TEXT,
    to_state         TEXT,
    reason           TEXT,
    detail           JSONB,
    error            TEXT,
    operator_id      TEXT,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, stock_prepare_id) REFERENCES stock_prepares(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_events_tenant_stock ON stock_prepare_events (tenant_id, stock_prepare_id, id);

CREATE TABLE IF NOT EXISTS shipment_events (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    shipment_id      TEXT NOT NULL,
    stock_prepare_id TEXT,
    order_id         TEXT NOT NULL,
    stage_code       TEXT NOT NULL,
    task_id          TEXT,
    action           TEXT NOT NULL,
    from_state       TEXT,
    to_state         TEXT,
    reason           TEXT,
    detail           JSONB,
    error            TEXT,
    operator_id      TEXT,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, shipment_id) REFERENCES shipments(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_events_tenant_shipment ON shipment_events (tenant_id, shipment_id, id);

CREATE TABLE IF NOT EXISTS invoice_events (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    invoice_id       TEXT NOT NULL,
    shipment_id      TEXT,
    stock_prepare_id TEXT,
    order_id         TEXT NOT NULL,
    stage_code       TEXT NOT NULL,
    task_id          TEXT,
    action           TEXT NOT NULL,
    from_state       TEXT,
    to_state         TEXT,
    reason           TEXT,
    detail           JSONB,
    error            TEXT,
    operator_id      TEXT,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_events_tenant_invoice ON invoice_events (tenant_id, invoice_id, id);

-- 审批评论（CQ-05/12）
CREATE TABLE IF NOT EXISTS approval_comments (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    order_id        TEXT NOT NULL,
    scope           TEXT NOT NULL CHECK (scope IN ('ORD','STK','SHP','INV')),
    target_id       TEXT NOT NULL,
    author_id       TEXT NOT NULL,
    content         TEXT NOT NULL,
    attachments     JSONB,
    notify_user_ids JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders(tenant_id, id),
    FOREIGN KEY (tenant_id, author_id) REFERENCES users(tenant_id, id)
);

COMMIT;
