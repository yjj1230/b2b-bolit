package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NewPreOrderDraftVersion 是创建不可变草稿版本所需的完整快照。
type NewPreOrderDraftVersion struct {
	TenantID      string
	PreOrderID    string
	SourceMsgID   string
	SourceMsgHash string
	Quote         []byte
	Evidence      []byte
	Candidate     []byte
	PolicyStatus  string
	PolicyResult  []byte
	CustomerBasis []byte
	ProductBasis  []byte
	ContractBasis []byte
	PriceBasis    []byte
	ModelName     string
	ModelVersion  string
	PromptVersion string
	PolicyVersion string
	ExpiresAt     *time.Time
	Diff          []byte
	SnapshotJSON  []byte
	PatchJSON     []byte
	CreatedBy     string
	Status        string
}

func jsonOrEmpty(v []byte, fallback string) []byte {
	if len(v) == 0 {
		return []byte(fallback)
	}
	var raw json.RawMessage
	if json.Unmarshal(v, &raw) != nil {
		return []byte(fallback)
	}
	return v
}

// CreatePreOrderDraftVersion 创建新的草稿版本，不覆盖历史版本。
func (r *Repo) CreatePreOrderDraftVersion(ctx context.Context, in NewPreOrderDraftVersion) (*PreOrderDraft, error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var lockedID string
	if err := tx.tx.QueryRow(ctx, `
		SELECT id FROM pre_orders WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
		in.TenantID, in.PreOrderID).Scan(&lockedID); err != nil {
		return nil, fmt.Errorf("lock pre-order: %w", err)
	}
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(draft_version), 0) + 1
		FROM pre_order_drafts WHERE tenant_id=$1 AND pre_order_id=$2`,
		in.TenantID, in.PreOrderID).Scan(&version); err != nil {
		return nil, fmt.Errorf("next draft version: %w", err)
	}
	status := in.Status
	if status == "" {
		status = "CURRENT"
	}
	if status == "CURRENT" {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE pre_order_drafts SET status='SUPERSEDED'
			WHERE tenant_id=$1 AND pre_order_id=$2 AND status='CURRENT'`, in.TenantID, in.PreOrderID); err != nil {
			return nil, fmt.Errorf("supersede draft: %w", err)
		}
	}
	_, err = tx.tx.Exec(ctx, `
		INSERT INTO pre_order_drafts (
		 tenant_id, pre_order_id, draft_version, source_msg_id, source_msg_hash,
		 quote, evidence, candidate, policy_status, policy_result,
		 customer_basis, product_basis, contract_basis, price_basis,
		 model_name, model_version, prompt_version, policy_version, status, expires_at, diff,
		 snapshot_json, patch_json, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		in.TenantID, in.PreOrderID, version, in.SourceMsgID, in.SourceMsgHash,
		jsonOrEmpty(in.Quote, "{}"), jsonOrEmpty(in.Evidence, "[]"), jsonOrEmpty(in.Candidate, "{}"),
		in.PolicyStatus, jsonOrEmpty(in.PolicyResult, "{}"), jsonOrEmpty(in.CustomerBasis, "{}"),
		jsonOrEmpty(in.ProductBasis, "{}"), jsonOrEmpty(in.ContractBasis, "{}"), jsonOrEmpty(in.PriceBasis, "{}"),
		in.ModelName, in.ModelVersion, in.PromptVersion, in.PolicyVersion, status, in.ExpiresAt, jsonOrEmpty(in.Diff, "{}"),
		jsonOrEmpty(in.SnapshotJSON, "{}"), jsonOrEmpty(in.PatchJSON, "{}"), in.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("insert draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.GetCurrentPreOrderDraft(ctx, in.TenantID, in.PreOrderID)
}

// GetCurrentPreOrderDraft 查询当前草稿版本。
func (r *Repo) GetCurrentPreOrderDraft(ctx context.Context, tenantID, preOrderID string) (*PreOrderDraft, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT tenant_id, pre_order_id, draft_version, source_msg_id, source_msg_hash,
		 quote, evidence, candidate, policy_status, policy_result,
		 customer_basis, product_basis, contract_basis, price_basis,
		 model_name, model_version, prompt_version, policy_version, status, expires_at, diff,
		 snapshot_json, patch_json, created_by, created_at
		FROM pre_order_drafts WHERE tenant_id=$1 AND pre_order_id=$2 AND status='CURRENT'`, tenantID, preOrderID)
	return scanPreOrderDraft(row)
}

// GetPreOrderDraft 查询指定版本。
func (r *Repo) GetPreOrderDraft(ctx context.Context, tenantID, preOrderID string, version int64) (*PreOrderDraft, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT tenant_id, pre_order_id, draft_version, source_msg_id, source_msg_hash,
		 quote, evidence, candidate, policy_status, policy_result,
		 customer_basis, product_basis, contract_basis, price_basis,
		 model_name, model_version, prompt_version, policy_version, status, expires_at, diff,
		 snapshot_json, patch_json, created_by, created_at
		FROM pre_order_drafts WHERE tenant_id=$1 AND pre_order_id=$2 AND draft_version=$3`, tenantID, preOrderID, version)
	return scanPreOrderDraft(row)
}

func scanPreOrderDraft(row rowScanner) (*PreOrderDraft, error) {
	var d PreOrderDraft
	err := row.Scan(&d.TenantID, &d.PreOrderID, &d.DraftVersion, &d.SourceMsgID, &d.SourceMsgHash,
		&d.Quote, &d.Evidence, &d.Candidate, &d.PolicyStatus, &d.PolicyResult,
		&d.CustomerBasis, &d.ProductBasis, &d.ContractBasis, &d.PriceBasis,
		&d.ModelName, &d.ModelVersion, &d.PromptVersion, &d.PolicyVersion, &d.Status, &d.ExpiresAt, &d.Diff,
		&d.SnapshotJSON, &d.PatchJSON, &d.CreatedBy, &d.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, &ErrNotFound{What: "pre_order draft"}
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RecordPreOrderDecision 记录人工确认/修改/忽略；重复动作返回既有记录。
func (r *Repo) RecordPreOrderDecision(ctx context.Context, in PreOrderDecision) (*PreOrderDecision, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO pre_order_decisions (tenant_id, pre_order_id, draft_version, action, operator_id, reason, before_data, after_data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id, pre_order_id, draft_version, action) DO UPDATE SET operator_id=pre_order_decisions.operator_id
		RETURNING id, tenant_id, pre_order_id, draft_version, action, operator_id, reason, before_data, after_data, created_at`,
		in.TenantID, in.PreOrderID, in.DraftVersion, in.Action, in.OperatorID, in.Reason,
		jsonOrEmpty(in.BeforeData, "{}"), jsonOrEmpty(in.AfterData, "{}"))
	var out PreOrderDecision
	if err := row.Scan(&out.ID, &out.TenantID, &out.PreOrderID, &out.DraftVersion, &out.Action, &out.OperatorID,
		&out.Reason, &out.BeforeData, &out.AfterData, &out.CreatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordPolicyEvaluation 保存确定性策略输入和命中规则。
func (r *Repo) RecordPolicyEvaluation(ctx context.Context, in PolicyEvaluation) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO policy_evaluations (tenant_id, pre_order_id, draft_version, policy_version, input_snapshot, output, rule_hits, status, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, in.TenantID, in.PreOrderID, in.DraftVersion, in.PolicyVersion,
		jsonOrEmpty(in.InputSnapshot, "{}"), jsonOrEmpty(in.Output, "{}"), jsonOrEmpty(in.RuleHits, "[]"), in.Status, in.CorrelationID)
	return err
}
