package wecom

import (
	"context"
	"fmt"
	"strings"

	"b2b_pilot_v3/internal/repo"
)

// ── 凭证机器人（docs/v0.1/05 N7 / 06 §5.2 / 09-testing N-10）──
// 仓库群回单：仓管把签收回单发在企微仓库群 → 机器人自动抓取 → OCR 识别签收信息
// → 匹配订单 → 登记归档（evidence 只追加 v3，不覆盖）。
// 升级企微群前的手动上传备选入口 = 既有 POST .../evidence 端点（两路等价）。

// ReceiptInfo 回单识别结果（OCR；mock/真实识别器输出统一结构）。
type ReceiptInfo struct {
	OrderID    string  `json:"order_id"`
	ShipmentID string  `json:"shipment_id"`
	SignedAt   string  `json:"signed_at"`
	Qty        string  `json:"qty"`
	Confidence float64 `json:"confidence"`
}

// Recognizer 回单识别契约（OCR 接线点：mock 为确定性规则，真实实现换同一接口）。
type Recognizer interface {
	// RecognizeReceipt 从图片引用识别签收信息；识别失败 → ErrRecognitionFailed。
	RecognizeReceipt(ctx context.Context, imageURL, imageHash string) (ReceiptInfo, error)
}

// ErrRecognitionFailed 识别失败（人工补登路径）。
type ErrRecognitionFailed struct{ Reason string }

func (e *ErrRecognitionFailed) Error() string { return "receipt recognition failed: " + e.Reason }

// RuleRecognizer 确定性规则识别器（基线 mock：图片文件名携带发货单号；
// 生产由 OCR/LLM 实现替换，接口不变）。
type RuleRecognizer struct{}

// RecognizeReceipt 规则：图片 URL/哈希中形如 ship-<shipmentID> 的片段；
// 兼容 order-<orderID>（回退按订单匹配最新发货单由调用方处理）。
func (RuleRecognizer) RecognizeReceipt(ctx context.Context, imageURL, imageHash string) (ReceiptInfo, error) {
	hay := imageURL + "|" + imageHash
	lower := strings.ToLower(hay)
	if i := strings.Index(lower, "ship-"); i >= 0 {
		id := tokenAfter(hay, i, "ship-")
		return ReceiptInfo{ShipmentID: id, Confidence: 1.0}, nil
	}
	if i := strings.Index(lower, "order-"); i >= 0 {
		id := tokenAfter(hay, i, "order-")
		return ReceiptInfo{OrderID: id, Confidence: 0.9}, nil
	}
	return ReceiptInfo{}, &ErrRecognitionFailed{Reason: "no shipment/order token in image reference"}
}

// tokenAfter 提取 hay[i:] 后前缀 prefix 之后的第一个连续标识符片段。
func tokenAfter(hay string, i int, prefix string) string {
	rest := hay[i+len(prefix):]
	end := len(rest)
	for j, c := range rest {
		if !(c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			end = j
			break
		}
	}
	return rest[:end]
}

// ReceiptRobotStore 机器人仓储契约（repo.Repo 实现）。
type ReceiptRobotStore interface {
	LookupWarehouseGroupBinding(ctx context.Context, groupID string) (tenantID, warehouseID string, err error)
	GetShipment(ctx context.Context, tenantID, shipmentID string) (*repo.Shipment, error)
	AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta repo.EvidenceMeta) (int, error)
}

// ReceiptRobot 凭证机器人：仓库群回单抓取 → 识别 → 匹配 → 登记（只追加）。
type ReceiptRobot struct {
	Store      ReceiptRobotStore
	Recognizer Recognizer
	// MinConfidence OCR-01 收敛：低于阈值的识别结果不自动登记（人工补登）；
	// 规则识别器基线置信度 1.0/0.9，真实 OCR 接入后按业务调阈值。
	MinConfidence float64
}

// NewReceiptRobot 构造（默认规则识别器；置信度阈值 0.5——低于 0.5 拒绝自动登记）。
func NewReceiptRobot(store ReceiptRobotStore) *ReceiptRobot {
	return &ReceiptRobot{Store: store, Recognizer: RuleRecognizer{}, MinConfidence: 0.5}
}

// CaptureResult 抓取登记结果（N-10 断言：版本只追加、匹配订单）。
type CaptureResult struct {
	TenantID    string `json:"tenant_id"`
	WarehouseID string `json:"warehouse_id"`
	OrderID     string `json:"order_id"`
	ShipmentID  string `json:"shipment_id"`
	Version     int    `json:"version"`
	Source      string `json:"source"`
}

// CaptureFromGroup 从仓库群抓取一张回单图片并登记：
//  1. 群必须绑定为仓库群（kind=warehouse_group），否则 ErrNotWarehouseGroup；
//  2. OCR 识别签收信息（失败 → ErrRecognitionFailed，人工补登）；
//  3. 匹配发货单（须属于该租户；仓库维度匹配为基线校验）；
//  4. evidence 只追加登记（kind=RECEIPT, source=robot, uploader=robot）。
func (rb *ReceiptRobot) CaptureFromGroup(ctx context.Context, groupID, imageURL, imageHash string) (*CaptureResult, error) {
	tenantID, warehouseID, err := rb.Store.LookupWarehouseGroupBinding(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("not a warehouse group: %w", err)
	}
	info, err := rb.Recognizer.RecognizeReceipt(ctx, imageURL, imageHash)
	if err != nil {
		return nil, err
	}
	if rb.MinConfidence > 0 && info.Confidence < rb.MinConfidence {
		return nil, &ErrRecognitionFailed{Reason: fmt.Sprintf("low confidence %.2f < %.2f (人工补登)", info.Confidence, rb.MinConfidence)}
	}
	if info.ShipmentID == "" {
		return nil, &ErrRecognitionFailed{Reason: "no shipment matched"}
	}
	// 识别 token 可能携带多余后缀（ship-SHP-0112-receipt → SHP-0112）；
	// 逐段回退匹配真实发货单（基线规则识别器容错）。
	id := info.ShipmentID
	var shp *repo.Shipment
	for {
		var err error
		shp, err = rb.Store.GetShipment(ctx, tenantID, id)
		if err == nil {
			break
		}
		if _, ok := err.(*repo.ErrNotFound); !ok {
			return nil, fmt.Errorf("match shipment: %w", err)
		}
		if i := strings.LastIndex(id, "-"); i > 0 {
			id = id[:i]
			continue
		}
		return nil, fmt.Errorf("match shipment: %w", err)
	}
	info.ShipmentID = id
	// 基线：本仓匹配校验（跨仓回单拒绝登记）
	if warehouseID != "" && shp.WarehouseID != warehouseID {
		return nil, fmt.Errorf("receipt warehouse %s does not match shipment warehouse %s", warehouseID, shp.WarehouseID)
	}
	ver, err := rb.Store.AppendEvidenceMeta(ctx, tenantID, info.ShipmentID, "robot", imageURL, imageHash, repo.EvidenceMeta{
		Kind: "RECEIPT", Source: "robot",
	})
	if err != nil {
		return nil, err
	}
	return &CaptureResult{
		TenantID: tenantID, WarehouseID: warehouseID,
		OrderID: shp.OrderID, ShipmentID: info.ShipmentID, Version: ver, Source: "robot",
	}, nil
}
