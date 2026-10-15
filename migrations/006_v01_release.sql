-- 006_v01_release.sql —— 0.1 发布版结构补齐（docs/v0.1/07-database.html）
-- 设计来源：docs/v0.1/07-database.html §3 + gap-analysis.md A1/A4/A6
--  A1  收货地址：order_items.receive_address（商务备货按收货地址选仓；锁库存暂不做）
--  A4  多单合并开票：invoices 三金额分展（收款/发货/开票）+ invoice_order_links 关联表
--  A6  凭证机器人：evidence_versions 识别登记字段（kind/source/invoice_no/amount/tax）
-- 幂等原则：全部 IF NOT EXISTS / ADD COLUMN IF NOT EXISTS，可重复执行。

BEGIN;

-- ── A1：订单明细收货地址（选仓依据；空串 = 未填，0.1 允许为空）──
ALTER TABLE order_items ADD COLUMN IF NOT EXISTS receive_address TEXT NOT NULL DEFAULT '';

-- ── A4：发票三金额分展（收款 received / 发货 shipped / 开票 invoiced）──
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS received_cents BIGINT NOT NULL DEFAULT 0 CHECK (received_cents >= 0);
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS shipped_cents  BIGINT NOT NULL DEFAULT 0 CHECK (shipped_cents >= 0);
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS invoiced_cents BIGINT NOT NULL DEFAULT 0 CHECK (invoiced_cents >= 0);

-- ── A4：发票-订单多对多关联（一笔款跨多笔订单；关联可追加，漏了可补）──
CREATE TABLE IF NOT EXISTS invoice_order_links (
    tenant_id    TEXT NOT NULL,
    invoice_id   TEXT NOT NULL,
    order_id     TEXT NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    qty          BIGINT NOT NULL CHECK (qty > 0),
    linked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 同一发票同一订单只关联一次（重复追加幂等）；同一订单可被多张发票关联
    PRIMARY KEY (tenant_id, invoice_id, order_id),
    FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices(tenant_id, id),
    FOREIGN KEY (tenant_id, order_id)   REFERENCES orders(tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_invoice_links_order ON invoice_order_links (tenant_id, order_id);

-- ── A6/CW1：凭证识别登记字段（主路径后台上传 + 凭证机器人抓取共用）──
ALTER TABLE evidence_versions ADD COLUMN IF NOT EXISTS kind        TEXT NOT NULL DEFAULT 'GENERAL';
ALTER TABLE evidence_versions ADD COLUMN IF NOT EXISTS source      TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE evidence_versions ADD COLUMN IF NOT EXISTS invoice_no  TEXT NOT NULL DEFAULT '';
ALTER TABLE evidence_versions ADD COLUMN IF NOT EXISTS amount_cents BIGINT NOT NULL DEFAULT 0 CHECK (amount_cents >= 0);
ALTER TABLE evidence_versions ADD COLUMN IF NOT EXISTS tax_cents   BIGINT NOT NULL DEFAULT 0 CHECK (tax_cents >= 0);

COMMIT;
