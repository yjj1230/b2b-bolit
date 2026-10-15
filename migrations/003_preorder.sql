-- 003_preorder.sql —— 企微域：预订单/预订单事件链/企微上下文/存档游标
-- 设计来源：docs/02-architecture.html §3.1 + docs/07-wecom-integration.html §2/§4

BEGIN;

-- 预订单（CQ-11；防线 3：消息幂等 UNIQUE(tenant,group,msg_id) + 订单幂等 UNIQUE(tenant,idempotency_key)）
CREATE TABLE IF NOT EXISTS pre_orders (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    group_id        TEXT NOT NULL,
    msg_id          TEXT NOT NULL,
    state           TEXT NOT NULL,
    candidates      JSONB NOT NULL DEFAULT '[]',
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, group_id, msg_id),
    UNIQUE (tenant_id, idempotency_key),
    FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);

-- 预订单事件链（append-only，防线 6；CQ-11/12；五类事件表同构：含 action 列）
CREATE TABLE IF NOT EXISTS pre_order_events (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    pre_order_id TEXT NOT NULL,
    action       TEXT NOT NULL,
    from_state   TEXT,
    to_state     TEXT,
    reason       TEXT,
    detail       JSONB,
    error        TEXT,
    operator_id  TEXT,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, pre_order_id) REFERENCES pre_orders(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_pre_events_tenant_pre ON pre_order_events (tenant_id, pre_order_id, id);

-- 企微上下文绑定（07 §4：客户群/外部用户 → 租户 + 归属销售；出站按租户核验）
CREATE TABLE IF NOT EXISTS wecom_context_bindings (
    tenant_id        TEXT NOT NULL,
    external_user_id TEXT,
    group_id         TEXT,
    binding          JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (tenant_id, external_user_id, group_id),
    FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);
CREATE INDEX IF NOT EXISTS idx_wecom_binding_group ON wecom_context_bindings (tenant_id, group_id);

-- 会话存档游标（07 §2.3：原子 checkpoint，5 天窗口防线；L1-14）
CREATE TABLE IF NOT EXISTS wecom_archive_cursor (
    tenant_id  TEXT PRIMARY KEY,
    seq        BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);

COMMIT;
