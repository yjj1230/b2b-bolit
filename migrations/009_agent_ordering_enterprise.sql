-- 009_agent_ordering_enterprise.sql
-- Agent 下单企业级审计、版本化草稿、工作流启动 outbox 和死信。

BEGIN;

CREATE TABLE IF NOT EXISTS pre_order_drafts (
    tenant_id        TEXT NOT NULL,
    pre_order_id     TEXT NOT NULL,
    draft_version    BIGINT NOT NULL CHECK (draft_version > 0),
    source_msg_id    TEXT NOT NULL,
    source_msg_hash  TEXT NOT NULL,
    quote            JSONB NOT NULL DEFAULT '{}',
    evidence         JSONB NOT NULL DEFAULT '[]',
    candidate        JSONB NOT NULL DEFAULT '{}',
    policy_status    TEXT NOT NULL DEFAULT 'PENDING',
    policy_result    JSONB NOT NULL DEFAULT '{}',
    customer_basis   JSONB NOT NULL DEFAULT '{}',
    product_basis    JSONB NOT NULL DEFAULT '{}',
    contract_basis   JSONB NOT NULL DEFAULT '{}',
    price_basis      JSONB NOT NULL DEFAULT '{}',
    model_name       TEXT NOT NULL DEFAULT '',
    model_version    TEXT NOT NULL DEFAULT '',
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'CURRENT',
    expires_at       TIMESTAMPTZ,
    diff             JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, pre_order_id, draft_version),
    FOREIGN KEY (tenant_id, pre_order_id) REFERENCES pre_orders(tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_pre_order_current_draft
    ON pre_order_drafts (tenant_id, pre_order_id) WHERE status = 'CURRENT';

CREATE TABLE IF NOT EXISTS pre_order_decisions (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    pre_order_id   TEXT NOT NULL,
    draft_version  BIGINT NOT NULL,
    action         TEXT NOT NULL,
    operator_id    TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    before_data    JSONB NOT NULL DEFAULT '{}',
    after_data     JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, pre_order_id, draft_version, action),
    FOREIGN KEY (tenant_id, pre_order_id, draft_version)
        REFERENCES pre_order_drafts(tenant_id, pre_order_id, draft_version)
);

CREATE TABLE IF NOT EXISTS policy_evaluations (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    pre_order_id   TEXT NOT NULL,
    draft_version  BIGINT NOT NULL,
    policy_version TEXT NOT NULL,
    input_snapshot JSONB NOT NULL DEFAULT '{}',
    output         JSONB NOT NULL DEFAULT '{}',
    rule_hits      JSONB NOT NULL DEFAULT '[]',
    status         TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, pre_order_id, draft_version)
        REFERENCES pre_order_drafts(tenant_id, pre_order_id, draft_version)
);
CREATE INDEX IF NOT EXISTS idx_policy_eval_lookup
    ON policy_evaluations (tenant_id, pre_order_id, draft_version, created_at DESC);

CREATE TABLE IF NOT EXISTS workflow_outbox_events (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    event_key      TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    aggregate_id   TEXT NOT NULL,
    workflow_id    TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    status         TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','PROCESSING','DONE','FAILED','DEAD')),
    attempts       INT NOT NULL DEFAULT 0,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until   TIMESTAMPTZ,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at   TIMESTAMPTZ,
    UNIQUE (tenant_id, event_key)
);
CREATE INDEX IF NOT EXISTS idx_workflow_outbox_claim
    ON workflow_outbox_events (status, available_at, id);
CREATE INDEX IF NOT EXISTS idx_workflow_outbox_aggregate
    ON workflow_outbox_events (tenant_id, aggregate_id, status);

CREATE TABLE IF NOT EXISTS dead_letter_events (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    source         TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    error          TEXT NOT NULL,
    attempts       INT NOT NULL DEFAULT 0,
    resolved       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    resolved_by    TEXT
);
CREATE INDEX IF NOT EXISTS idx_dead_letter_pending
    ON dead_letter_events (tenant_id, resolved, created_at);

COMMIT;
