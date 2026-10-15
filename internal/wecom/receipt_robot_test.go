package wecom

import (
	"context"
	"errors"
	"testing"

	"b2b_pilot_v3/internal/repo"
)

// ── N-10 · 仓库群回单机器人（A6）单元测试（纯库，无 PG）──
// 抓取 → 规则识别（OCR 接线点）→ 匹配发货单 → 登记 v3（只追加不覆盖）。

type robotFakeStore struct {
	bindingErr error
	tenant     string
	warehouse  string
	shipment   *repo.Shipment
	versions   []repo.EvidenceVersion
}

func (f *robotFakeStore) LookupWarehouseGroupBinding(ctx context.Context, groupID string) (string, string, error) {
	if f.bindingErr != nil {
		return "", "", f.bindingErr
	}
	return f.tenant, f.warehouse, nil
}

func (f *robotFakeStore) GetShipment(ctx context.Context, tenantID, shipmentID string) (*repo.Shipment, error) {
	if f.shipment == nil || f.shipment.ID != shipmentID {
		return nil, &repo.ErrNotFound{What: "shipment " + shipmentID}
	}
	return f.shipment, nil
}

func (f *robotFakeStore) AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta repo.EvidenceMeta) (int, error) {
	ver := len(f.versions) + 1
	f.versions = append(f.versions, repo.EvidenceVersion{
		TenantID: tenantID, ShipmentID: shipmentID, Version: ver, UploaderID: uploaderID,
		FileURL: fileURL, FileHash: fileHash, Kind: meta.Kind, Source: meta.Source,
	})
	return ver, nil
}

func newRobotFake() *robotFakeStore {
	return &robotFakeStore{
		tenant: "chang-an", warehouse: "wh-yangyuan",
		shipment: &repo.Shipment{ID: "SHP-0112", TenantID: "chang-an", OrderID: "ORD-0112", WarehouseID: "wh-yangyuan"},
	}
}

// 机器人从仓库群抓取回单：识别 → 匹配 → 登记 v3（只追加）。
func TestReceiptRobotCaptureFromGroup(t *testing.T) {
	f := newRobotFake()
	rb := NewReceiptRobot(f)

	// v1/v2 已由人工上传（升级企微群前的备选路径）
	for i := 1; i <= 2; i++ {
		if _, err := f.AppendEvidenceMeta(t.Context(), "chang-an", "SHP-0112", "u-op",
			"file://manual"+itoa(i)+".jpg", "h"+itoa(i), repo.EvidenceMeta{Kind: "RECEIPT", Source: "manual"}); err != nil {
			t.Fatalf("manual v%d: %v", i, err)
		}
	}

	// 仓库群回单 → 机器人识别（图片引用携带发货单号）→ 登记 v3
	res, err := rb.CaptureFromGroup(t.Context(), "group-wh-yangyuan",
		"https://wecom/ship-SHP-0112-receipt.jpg", "img-hash-3")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.ShipmentID != "SHP-0112" || res.OrderID != "ORD-0112" || res.Version != 3 {
		t.Fatalf("capture result: %+v (want v3)", res)
	}
	if res.Source != "robot" {
		t.Fatalf("source = %s, want robot", res.Source)
	}
	// 只追加：v1/v2 保留，版本单调
	if len(f.versions) != 3 || f.versions[0].Version != 1 || f.versions[1].Version != 2 || f.versions[2].Version != 3 {
		t.Fatalf("versions: %+v", f.versions)
	}
}

// 非仓库群 → 拒绝（静默忽略由回调层处理）。
func TestReceiptRobotNonWarehouseGroup(t *testing.T) {
	f := newRobotFake()
	f.bindingErr = &repo.ErrNotFound{What: "wecom binding for group other"}
	rb := NewReceiptRobot(f)
	if _, err := rb.CaptureFromGroup(t.Context(), "group-other", "u", "h"); err == nil {
		t.Fatal("non-warehouse group should be rejected")
	}
}

// 识别失败 → ErrRecognitionFailed（人工补登）。
func TestReceiptRobotRecognitionFailed(t *testing.T) {
	f := newRobotFake()
	rb := NewReceiptRobot(f)
	_, err := rb.CaptureFromGroup(t.Context(), "group-wh-yangyuan", "https://wecom/blurry.jpg", "h")
	var rf *ErrRecognitionFailed
	if err == nil || !errorsAs(err, &rf) {
		t.Fatalf("want ErrRecognitionFailed, got %v", err)
	}
}

// 跨仓回单 → 拒绝登记。
func TestReceiptRobotCrossWarehouseRejected(t *testing.T) {
	f := newRobotFake()
	f.warehouse = "wh-dongguan" // 群绑东莞仓，回单匹配阳原仓发货单
	rb := NewReceiptRobot(f)
	if _, err := rb.CaptureFromGroup(t.Context(), "group-wh-dongguan",
		"https://wecom/ship-SHP-0112.jpg", "h"); err == nil {
		t.Fatal("cross-warehouse receipt should be rejected")
	}
}

// errorsAs 简化 errors.As（避免 import errors 名冲突）。
func errorsAs(err error, target any) bool {
	switch t := target.(type) {
	case **ErrRecognitionFailed:
		if e, ok := err.(*ErrRecognitionFailed); ok {
			*t = e
			return true
		}
	}
	return false
}

// 规则识别器分支：order- 前缀（无发货单号）→ 识别结果无 ShipmentID → 失败。
func TestRuleRecognizerOrderOnly(t *testing.T) {
	info, err := (RuleRecognizer{}).RecognizeReceipt(t.Context(), "https://wecom/order-ORD-1.jpg", "h")
	if err != nil {
		t.Fatalf("order token should be recognized: %v", err)
	}
	if info.OrderID != "ORD-1" || info.ShipmentID != "" {
		t.Fatalf("unexpected info: %+v", info)
	}
	// 无 token → ErrRecognitionFailed
	if _, err := (RuleRecognizer{}).RecognizeReceipt(t.Context(), "https://wecom/blur.jpg", "h"); err == nil {
		t.Fatal("no token should fail")
	}
}

// OCR-01 收敛：低置信识别结果必须拒绝自动登记（人工补登）。
func TestReceiptRobotLowConfidenceRejected(t *testing.T) {
	f := newRobotFake()
	rb := NewReceiptRobot(f)
	rb.Recognizer = RecognizerFunc(func(ctx context.Context, imageURL, imageHash string) (ReceiptInfo, error) {
		return ReceiptInfo{ShipmentID: "SHP-0112", Confidence: 0.42}, nil
	})
	_, err := rb.CaptureFromGroup(t.Context(), "group-wh-yangyuan", "https://wecom/receipt.jpg", "h")
	if err == nil {
		t.Fatal("low confidence must fail (人工补登)")
	}
	var rf *ErrRecognitionFailed
	if !errors.As(err, &rf) {
		t.Fatalf("err = %v, want ErrRecognitionFailed", err)
	}
	if len(f.versions) != 0 {
		t.Fatalf("low confidence must not append evidence: %+v", f.versions)
	}
}

type RecognizerFunc func(ctx context.Context, imageURL, imageHash string) (ReceiptInfo, error)

func (fn RecognizerFunc) RecognizeReceipt(ctx context.Context, imageURL, imageHash string) (ReceiptInfo, error) {
	return fn(ctx, imageURL, imageHash)
}

// 抓取链路：发货单查询非 NotFound 错误 → 错误透传。
func TestReceiptRobotStoreErrorPropagates(t *testing.T) {
	f := newRobotFake()
	f.bindingErr = nil
	rb := NewReceiptRobot(f)
	// 识别出 ship- 但 store 报错（用未知 shipment → 回退裁剪后仍 NotFound）→ 匹配失败错误
	if _, err := rb.CaptureFromGroup(t.Context(), "group-wh-yangyuan", "https://wecom/ship-SHP-x.jpg", "h"); err == nil {
		t.Fatal("unmatched shipment should fail")
	}
}
