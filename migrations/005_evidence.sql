-- 凭证链（CQ-15）：签收单/回单只追加版本，无 UPDATE/DELETE 路径（代码层同样不暴露）
-- 回单状态由最新凭证版本推导，杜绝「篡改为已回」（EP-28-N3）
CREATE TABLE IF NOT EXISTS evidence_versions (
    tenant_id    TEXT NOT NULL,
    shipment_id  TEXT NOT NULL,
    version      INT  NOT NULL CHECK (version > 0),
    uploader_id  TEXT NOT NULL,
    file_url     TEXT NOT NULL,
    file_hash    TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 租户维度复合 PK/FK（02 §3.1 多租户隔离总则：唯一键与 FK 必须带租户维度）
    PRIMARY KEY (tenant_id, shipment_id, version),
    FOREIGN KEY (tenant_id, shipment_id) REFERENCES shipments(tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_evidence_tenant_shipment ON evidence_versions(tenant_id, shipment_id, version);
