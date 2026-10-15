-- 008_shadow.sql —— 影子全流程预演（docs/v0.1/12-shadow-full-flow.html §8.1）
-- 只进影子库 b2b_shadow：主 compose 的 migrate 只循环顶层 migrations/*.sql，
-- 不会进入本子目录（A-02 迁移目录拆分）；shadow-migrate 先应用 001-007 再应用本文件，
-- 且 DSN 必须先通过 shadowguard 启动守卫（库名必须 = b2b_shadow）。
-- 幂等：全部 IF NOT EXISTS / ON CONFLICT，可重复执行（可重建重放，S-铁5）。

BEGIN;

-- 影子建单标识：orders.submission_mode 增加 'shadow'（001 CHECK 原只允许 create/copy/resubmit）。
-- 只在影子库执行（本文件不会进入主 migrate 的顶层 glob，A-02）。
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_submission_mode_check;
ALTER TABLE orders ADD CONSTRAINT orders_submission_mode_check
    CHECK (submission_mode IN ('create','copy','resubmit','shadow'));

-- 影子库基础设施种子：影子租户（chang-an-shadow，A-03 与 A 轨租户空间隔离）与
-- 审批人路由基线（OrderWorkflow 的 approval_tasks 有 tenant+assignee 复合 FK；
-- 影子库不载入客户业务数据，但必须存在这些系统账号）。
-- 注意 users.id 是全局主键：影子用户用 u-shadow-* 前缀，与 A 轨 u-* 不冲突。
INSERT INTO tenants (id, name) VALUES ('chang-an-shadow', '长安·影子') ON CONFLICT DO NOTHING;
INSERT INTO users (id, tenant_id, name, role) VALUES
    ('u-shadow-supervisor', 'chang-an-shadow', '销售主管', 'SUPERVISOR'),
    ('u-shadow-chairman',   'chang-an-shadow', '董事长',   'CHAIRMAN'),
    ('u-shadow-op',         'chang-an-shadow', '商务',     'OP'),
    ('u-shadow-accounting', 'chang-an-shadow', '财务',     'ACCOUNTING'),
    ('u-shadow-sales',      'chang-an-shadow', '销售',     'SALES'),
    ('u-shadow-wh-yangyuan','chang-an-shadow', '阳原仓管', 'WAREHOUSE')
ON CONFLICT DO NOTHING;
-- stock_prepares.warehouse_id 有 tenant+warehouse 复合 FK；影子工作流需要仓库路由基线。
INSERT INTO warehouses (id, tenant_id, name) VALUES ('wh-yangyuan', 'chang-an-shadow', '阳原仓') ON CONFLICT DO NOTHING;

-- 运行档案：每次重建 / 重放的 run 记录
CREATE TABLE IF NOT EXISTS shadow_runs (
    run_id                TEXT PRIMARY KEY,
    ingest_config_digest  TEXT NOT NULL,
    feed_config_digest    TEXT NOT NULL,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at           TIMESTAMPTZ,
    status                TEXT NOT NULL DEFAULT 'RUNNING' CHECK (status IN ('RUNNING','COMPLETED','FAILED')),
    note                  TEXT NOT NULL DEFAULT ''
);

-- 旧系统真实事件原文（SanitizeRow 剥离后），只追加；是重放的输入（S-铁5）
CREATE TABLE IF NOT EXISTS shadow_source_events (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    legacy_flow_id   TEXT NOT NULL,
    legacy_event_id  TEXT NOT NULL,
    stage_code       TEXT NOT NULL DEFAULT '',
    action           TEXT NOT NULL,
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at      TIMESTAMPTZ,
    source           TEXT NOT NULL DEFAULT '',
    checksum         TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'NEW' CHECK (state IN ('NEW','FED','SKIPPED')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, legacy_flow_id, legacy_event_id)
);
CREATE INDEX IF NOT EXISTS idx_shadow_source_flow ON shadow_source_events (tenant_id, legacy_flow_id, id);
CREATE INDEX IF NOT EXISTS idx_shadow_source_state ON shadow_source_events (state, id);

-- 翻译与投递台账（NEW → DELIVERED / FAILED）
CREATE TABLE IF NOT EXISTS shadow_signal_log (
    id               BIGSERIAL PRIMARY KEY,
    source_event_id  BIGINT NOT NULL REFERENCES shadow_source_events(id),
    signal           TEXT NOT NULL,
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
    workflow_id      TEXT NOT NULL DEFAULT '',
    delivered_at     TIMESTAMPTZ,
    state            TEXT NOT NULL DEFAULT 'NEW' CHECK (state IN ('NEW','DELIVERED','FAILED')),
    error            TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_event_id, signal)
);
CREATE INDEX IF NOT EXISTS idx_shadow_signal_state ON shadow_signal_log (state, id);

-- 分歧清单：发现 → 分类 → 裁决 → 修复 → 重放验证（12 §9）
CREATE TABLE IF NOT EXISTS shadow_divergences (
    id             BIGSERIAL PRIMARY KEY,
    run_id         TEXT NOT NULL DEFAULT '',
    order_id       TEXT NOT NULL DEFAULT '',
    legacy_flow_id TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL CHECK (kind IN ('MAPPING','FSM_GAP','LEGACY_ANOMALY','DATA_GAP')),
    severity       TEXT NOT NULL DEFAULT 'P1' CHECK (severity IN ('P0','P1','P2')),
    reason         TEXT NOT NULL DEFAULT '',
    from_state     TEXT NOT NULL DEFAULT '',
    to_state       TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','DECIDED','CLOSED')),
    owner          TEXT NOT NULL DEFAULT '',
    fix_commit     TEXT NOT NULL DEFAULT '',
    replay_run_id  TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_shadow_divergence_status ON shadow_divergences (status, severity, kind);
-- 同一次运行内同流同类同原因只记录一条（幂等重跑不滚雪球）
CREATE UNIQUE INDEX IF NOT EXISTS uq_shadow_divergence_dedup
    ON shadow_divergences (run_id, legacy_flow_id, kind, reason, md5(detail));

-- FSM 边覆盖率：79 条迁移边的真实数据命中矩阵（12 §8.1）
CREATE TABLE IF NOT EXISTS shadow_coverage (
    fsm_name     TEXT NOT NULL,
    from_state   TEXT NOT NULL,
    to_state     TEXT NOT NULL,
    hit_count    BIGINT NOT NULL DEFAULT 0,
    first_hit_at TIMESTAMPTZ,
    last_hit_at  TIMESTAMPTZ,
    source       TEXT NOT NULL DEFAULT 'real',  -- real | seed
    PRIMARY KEY (fsm_name, from_state, to_state)
);

COMMIT;
