-- 004_outbox.sql —— Outbox 投影器（rebac 决策 D4；docs/02-architecture.html §2.3）
-- Transactional Outbox：业务事务内写业务数据 + outbox_event；投影器异步消费 → SpiceDB。
-- 幂等：投影器按 event id 标记 PROCESSED，重复消费不产生重复关系（Touch 幂等）。

BEGIN;

-- 投影事件表
CREATE TABLE IF NOT EXISTS outbox_events (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    aggregate   TEXT NOT NULL,          -- prestock / shipment / customer / delegation
    aggregate_id TEXT NOT NULL,
    event_type  TEXT NOT NULL,          -- prestock.created / shipment.created /
                                        -- customer.contact_upsert / delegation.grant / delegation.revoke
    payload     JSONB NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_status ON outbox_events (status, id);

-- 客户联系人事实表（rebac 场景 D/H；customer#contact 的业务数据源）
CREATE TABLE IF NOT EXISTS customer_contacts (
    tenant_id   TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, customer_id),
    FOREIGN KEY (tenant_id, customer_id) REFERENCES customers(tenant_id, id)
);

COMMIT;
