package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BridgeSubmission 桥接下单回写记录（v0.1 自建单台账；只作视图，不启动 b2b 工作流）。
// JSON 字段名与前端契约一致（order_id/flow_id/instance_id/...）。
type BridgeSubmission struct {
	OrderID    string `json:"order_id"`
	FlowID     string `json:"flow_id"`
	InstanceID string `json:"instance_id"`
	StageID    string `json:"stage_id"`
	DetailURL  string `json:"detail_url"`
	State      string `json:"state"`
	CreatedAt  string `json:"created_at"`
}

// RecordBridgeSubmission 幂等回写：orders 台账行 + legacy_flow_mapping。
// v0.1 第 1 次发布口径：订单在旧飞书系统执行，本表仅承载「我的订单」列表与状态回显。
func (r *Repo) RecordBridgeSubmission(ctx context.Context, tenantID, orderID, flowID, instanceID, stageID, detailURL string) error {
	if tenantID == "" || orderID == "" || flowID == "" || instanceID == "" {
		return fmt.Errorf("record bridge submission: tenant/order/flow/instance required")
	}
	// 旧系统结算方式多种文本；本表 check 只允许 款到/非款到，台账视图统一记非款到。
	paymentTerm := "非款到"
	idemKey := "B2B_V01_" + tenantID + ":" + orderID
	payload := MarshalJSONDetail(map[string]any{
		"bridge_submission": true,
		"flow_id":           flowID,
		"instance_id":       instanceID,
		"stage_id":          stageID,
		"detail_url":        detailURL,
	})
	stageCode := "ORD"
	if i := strings.LastIndex(stageID, "-"); i >= 0 {
		stageCode = strings.TrimSpace(stageID[i+1:])
		if len(stageCode) > 8 {
			stageCode = "ORD"
		}
	}
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO orders (id, tenant_id, customer_id, state, payment_term, supplier_channel,
		                    idempotency_key, payload_hash, payload, legacy_flow_id, submission_mode)
		VALUES ($1,$2,$3,$4,$5,'', $6,$7,$8,$9,'create')
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE
		SET legacy_flow_id = EXCLUDED.legacy_flow_id,
		    payload = EXCLUDED.payload,
		    payload_hash = EXCLUDED.payload_hash,
		    updated_at = now()`,
		orderID, tenantID, "bridge-customer", "ORD_REQUESTED", paymentTerm,
		idemKey, HashPayload(payload), payload, flowID); err != nil {
		return fmt.Errorf("record bridge order: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO legacy_flow_mapping (tenant_id, legacy_flow_id, legacy_stage_id, stage_code,
		                                legacy_instance_id, target_type, target_order_id)
		VALUES ($1,$2,$3,$4,$5,'order',$6)
		ON CONFLICT (tenant_id, stage_code, legacy_instance_id) DO UPDATE
		SET legacy_flow_id = EXCLUDED.legacy_flow_id,
		    legacy_stage_id = EXCLUDED.legacy_stage_id,
		    target_order_id = EXCLUDED.target_order_id,
		    imported_at = now()`,
		tenantID, flowID, stageID, stageCode, instanceID, orderID); err != nil {
		return fmt.Errorf("record bridge mapping: %w", err)
	}
	return tx.Commit(ctx)
}

// RecordSubmission 实现 bridge.Recorder 契约。
func (r *Repo) RecordSubmission(ctx context.Context, tenantID, orderID, flowID, instanceID, stageID, detailURL string) error {
	return r.RecordBridgeSubmission(ctx, tenantID, orderID, flowID, instanceID, stageID, detailURL)
}

// ListBridgeSubmissions 列出桥接自建单台账（最近优先）。
func (r *Repo) ListBridgeSubmissions(ctx context.Context, tenantID string, limit int) ([]BridgeSubmission, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, legacy_flow_id, state, payload, created_at::text
		FROM orders
		WHERE tenant_id=$1 AND legacy_flow_id IS NOT NULL AND payload->>'bridge_submission' = 'true'
		ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BridgeSubmission
	for rows.Next() {
		var s BridgeSubmission
		var raw []byte
		if err := rows.Scan(&s.OrderID, &s.FlowID, &s.State, &raw, &s.CreatedAt); err != nil {
			return nil, err
		}
		var pl struct {
			InstanceID string `json:"instance_id"`
			StageID    string `json:"stage_id"`
			DetailURL  string `json:"detail_url"`
		}
		if err := json.Unmarshal(raw, &pl); err == nil {
			s.InstanceID = pl.InstanceID
			s.StageID = pl.StageID
			s.DetailURL = pl.DetailURL
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
