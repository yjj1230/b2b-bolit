package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EvidenceVersion 凭证版本（签收单/回单；只追加，CQ-15）。
type EvidenceVersion struct {
	TenantID   string    `json:"tenant_id"`
	ShipmentID string    `json:"shipment_id"`
	Version    int       `json:"version"`
	UploaderID string    `json:"uploader_id"`
	FileURL    string    `json:"file_url"`
	FileHash   string    `json:"file_hash"`
	CreatedAt  time.Time `json:"created_at"`
	// A6/CW1 识别登记字段：kind(INVOICE/RECEIPT/GENERAL)、source(manual/robot)、
	// 发票号/金额/税额（凭证机器人 OCR 与后台上传共用；只追加不覆盖）。
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	InvoiceNo   string `json:"invoice_no"`
	AmountCents int64  `json:"amount_cents"`
	TaxCents    int64  `json:"tax_cents"`
}

// EvidenceMeta 凭证识别登记元数据（追加时携带）。
type EvidenceMeta struct {
	Kind        string // INVOICE / RECEIPT / GENERAL
	Source      string // manual / robot
	InvoiceNo   string
	AmountCents int64
	TaxCents    int64
}

// AppendEvidenceTx 追加凭证下一版本（行锁防并发版本冲突；shipment 不存在则拒绝）。
// 只追加纪律：本方法为唯一写入入口，无 Update/Delete 路径。
func (r *Repo) AppendEvidenceTx(ctx context.Context, tx *Tx, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta EvidenceMeta) (int, error) {
	if meta.Kind == "" {
		meta.Kind = "GENERAL"
	}
	if meta.Source == "" {
		meta.Source = "manual"
	}
	var ver int
	// 锁 shipment 行使同一发货单的追加串行化（与 ApplyShipment 行锁同模式）；MAX+1 在锁内计算
	var one int
	if err := tx.tx.QueryRow(ctx,
		`SELECT 1 FROM shipments WHERE id=$1 AND tenant_id=$2 FOR UPDATE`,
		shipmentID, tenantID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("shipment %s not found in tenant %s", shipmentID, tenantID)
		}
		return 0, err
	}
	if err := tx.tx.QueryRow(ctx,
		`INSERT INTO evidence_versions (tenant_id, shipment_id, version, uploader_id, file_url, file_hash,
		                                 kind, source, invoice_no, amount_cents, tax_cents)
		 SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4, $5, $6, $7, $8, $9, $10
		 FROM evidence_versions WHERE shipment_id = $2
		 RETURNING version`,
		tenantID, shipmentID, uploaderID, fileURL, fileHash,
		meta.Kind, meta.Source, meta.InvoiceNo, meta.AmountCents, meta.TaxCents).Scan(&ver); err != nil {
		return 0, err
	}
	// D-04：回单凭证登记即闭合「限时放行补齐回单」待办（例外链闭合）。
	if meta.Kind == "RECEIPT" {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE approval_tasks SET state='APPROVED', verdict='RECEIPT_PROVIDED',
			       decided_by=$1, decided_at=now()
			WHERE tenant_id=$2 AND kind='RECEIPT_FOLLOWUP' AND state='PENDING'
			  AND order_id = (SELECT order_id FROM shipments WHERE tenant_id=$2 AND id=$3)`,
			uploaderID, tenantID, shipmentID); err != nil {
			return 0, err
		}
	}
	return ver, nil
}

// AppendEvidence 追加凭证（自开事务封装；供 API/Store 使用；无识别元数据）。
func (r *Repo) AppendEvidence(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string) (int, error) {
	return r.AppendEvidenceMeta(ctx, tenantID, shipmentID, uploaderID, fileURL, fileHash, EvidenceMeta{})
}

// AppendEvidenceMeta 追加凭证（携带识别登记元数据；主路径后台上传与凭证机器人共用）。
func (r *Repo) AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta EvidenceMeta) (int, error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	ver, err := r.AppendEvidenceTx(ctx, tx, tenantID, shipmentID, uploaderID, fileURL, fileHash, meta)
	if err != nil {
		return 0, err
	}
	return ver, tx.Commit(ctx)
}

// ListEvidenceVersions 回放全部版本（升序；CQ-15「任一版本可回放」）。
func (r *Repo) ListEvidenceVersions(ctx context.Context, tenantID, shipmentID string) ([]EvidenceVersion, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT tenant_id, shipment_id, version, uploader_id, file_url, file_hash, created_at,
		        kind, source, invoice_no, amount_cents, tax_cents
		 FROM evidence_versions WHERE tenant_id=$1 AND shipment_id=$2 ORDER BY version`, tenantID, shipmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvidenceVersion{}
	for rows.Next() {
		var v EvidenceVersion
		if err := rows.Scan(&v.TenantID, &v.ShipmentID, &v.Version, &v.UploaderID, &v.FileURL, &v.FileHash, &v.CreatedAt,
			&v.Kind, &v.Source, &v.InvoiceNo, &v.AmountCents, &v.TaxCents); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
