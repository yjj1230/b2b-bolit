-- 010_agent_multiturn_ordering.sql
-- 多轮消息事实、订单会话归并和草稿来源审计。

BEGIN;

ALTER TABLE pre_orders
    ADD COLUMN IF NOT EXISTS conversation_id TEXT,
    ADD COLUMN IF NOT EXISTS root_msg_id TEXT,
    ADD COLUMN IF NOT EXISTS confirmed_draft_version BIGINT;

ALTER TABLE pre_order_drafts
    ADD COLUMN IF NOT EXISTS snapshot_json JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS patch_json JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT 'agent';

CREATE TABLE IF NOT EXISTS inbound_messages (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    group_id         TEXT NOT NULL,
    msg_id           TEXT NOT NULL,
    sender_id        TEXT NOT NULL DEFAULT '',
    message_seq      BIGINT,
    reply_to_msg_id  TEXT,
    raw_text         TEXT NOT NULL DEFAULT '',
    text_hash        TEXT NOT NULL DEFAULT '',
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    parse_status     TEXT NOT NULL DEFAULT 'RECEIVED',
    retry_count      INT NOT NULL DEFAULT 0,
    session_id       TEXT,
    draft_version    BIGINT,
    correlation_id   TEXT NOT NULL,
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, group_id, msg_id),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);
CREATE INDEX IF NOT EXISTS idx_inbound_messages_session
    ON inbound_messages (tenant_id, session_id, received_at);
CREATE INDEX IF NOT EXISTS idx_inbound_messages_status
    ON inbound_messages (tenant_id, parse_status, received_at);

CREATE TABLE IF NOT EXISTS order_conversations (
    session_id             TEXT PRIMARY KEY,
    tenant_id              TEXT NOT NULL,
    group_id               TEXT NOT NULL,
    sender_id              TEXT NOT NULL DEFAULT '',
    customer_id            TEXT,
    owner_sales_id         TEXT,
    pre_order_id           TEXT NOT NULL,
    current_draft_version BIGINT NOT NULL DEFAULT 0,
    last_draft_version    BIGINT NOT NULL DEFAULT 0,
    confirmed_draft_version BIGINT,
    status                 TEXT NOT NULL DEFAULT 'OPEN',
    last_message_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    escalate_at           TIMESTAMPTZ,
    expires_at             TIMESTAMPTZ NOT NULL,
    correlation_id         TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, session_id),
    UNIQUE (tenant_id, pre_order_id),
    FOREIGN KEY (tenant_id, pre_order_id) REFERENCES pre_orders(tenant_id, id),
    FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);
CREATE INDEX IF NOT EXISTS idx_order_conversations_open
    ON order_conversations (tenant_id, group_id, sender_id, status, expires_at);

ALTER TABLE pre_orders
    ADD CONSTRAINT fk_pre_orders_conversation
    FOREIGN KEY (tenant_id, conversation_id)
    REFERENCES order_conversations(tenant_id, session_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE IF NOT EXISTS pre_order_draft_sources (
    tenant_id      TEXT NOT NULL,
    pre_order_id   TEXT NOT NULL,
    draft_version  BIGINT NOT NULL,
    message_id     BIGINT NOT NULL,
    source_role    TEXT NOT NULL DEFAULT 'INPUT',
    quote          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, pre_order_id, draft_version, message_id),
    FOREIGN KEY (tenant_id, pre_order_id, draft_version)
        REFERENCES pre_order_drafts(tenant_id, pre_order_id, draft_version),
    FOREIGN KEY (tenant_id, message_id)
        REFERENCES inbound_messages(tenant_id, id)
);

COMMIT;
