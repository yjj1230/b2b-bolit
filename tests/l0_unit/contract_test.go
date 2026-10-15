// Package l0unit —— L0 · 纯 Go 黑盒契约测试（docs/03-testing.html L0；零外部依赖）
//
// 与 internal/domain 白盒穷举（L0-01..10）互补：本套件从 tests/ 视角以导出 API
// 断言「文档契约」——FSM 迁移表/守恒断言/幂等键规范/回单分流/接入层默认值，
// 任何一侧漂移即失败（roundtrip 纪律：文档 ↔ 代码）。
package l0unit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/repo"
)

// ── FSM 契约（01 §2.2-2.6 状态图；非法迁移必须拒绝）──

func TestFSMContracts(t *testing.T) {
	// 文档状态图的关键边（抽样契约，穷举见 internal/domain L0-01..05）
	edges := []struct {
		name string
		fsm  *domain.FSM
		from string
		to   string
	}{
		// ORD（§2.2）
		{"ORD 校验通过", domain.OrdFSM, domain.StateORDRequested, domain.StateORDPriceReviewing},
		{"ORD 校验失败", domain.OrdFSM, domain.StateORDRequested, domain.StateORDRejected},
		{"ORD 主管通过", domain.OrdFSM, domain.StateORDPriceReviewing, domain.StateORDPriceApproved},
		{"ORD 退回修改", domain.OrdFSM, domain.StateORDPriceReviewing, domain.StateORDReturned},
		{"ORD A/B 级跳过董事长", domain.OrdFSM, domain.StateORDPriceApproved, domain.StateORDContractGenerate},
		{"ORD C/D 级进董事长", domain.OrdFSM, domain.StateORDPriceApproved, domain.StateORDChairmanReview},
		{"ORD 修改 A 类回审价", domain.OrdFSM, domain.StateORDModifying, domain.StateORDPriceReviewing},
		{"ORD 修改 B/D 类直达发货", domain.OrdFSM, domain.StateORDModifying, domain.StateSHPShipAssigning},
		{"ORD 修改 E 类静默", domain.OrdFSM, domain.StateORDModifying, domain.StateORDContracted},
		{"ORD 退回重提", domain.OrdFSM, domain.StateORDReturned, domain.StateORDPriceReviewing},
		// STK（§2.3）
		{"STK 款到进备货", domain.StkFSM, domain.StateSTKReadyForStock, domain.StateSTKStockReviewing},
		{"STK 非款到直达发货", domain.StkFSM, domain.StateSTKReadyForStock, domain.StateSHPShipAssigning},
		{"STK 仓管确认", domain.StkFSM, domain.StateSTKWarehouseConfirm, domain.StateSTKWarehouseConfirmed},
		{"STK 仓管申请转仓", domain.StkFSM, domain.StateSTKWarehouseConfirm, domain.StateSTKTransferRequested},
		{"STK 同意原仓", domain.StkFSM, domain.StateSTKTransferRequested, domain.StateSTKWarehouseConfirmed},
		{"STK 财务确认到账", domain.StkFSM, domain.StateSTKPaymentConfirming, domain.StateSTKPaymentConfirmed},
		{"STK 发货命令", domain.StkFSM, domain.StateSTKShipOrderIssued, domain.StateSHPShipAssigning},
		// SHP（§2.4）
		{"SHP 提交实发", domain.ShpFSM, domain.StateSHPShipAssigning, domain.StateSHPWarehouseShip},
		{"SHP 实发完成（信号直达，05 §3 校准）", domain.ShpFSM, domain.StateSHPShipAssigning, domain.StateSHPShipPed},
		{"SHP 实发完成", domain.ShpFSM, domain.StateSHPWarehouseShip, domain.StateSHPShipPed},
		{"SHP 全部发货", domain.ShpFSM, domain.StateSHPShipPed, domain.StateSHPFullyShipped},
		{"SHP 部分发货", domain.ShpFSM, domain.StateSHPShipPed, domain.StateSHPPartialShipped},
		{"SHP 部分继续", domain.ShpFSM, domain.StateSHPPartialShipped, domain.StateSHPShipAssigning},
		{"SHP 部分申请开票", domain.ShpFSM, domain.StateSHPPartialShipped, domain.StateINVReadyForInvoice},
		// INV（§2.5）
		{"INV 直接开票申请", domain.InvFSM, domain.StateINVReadyForInvoice, domain.StateINVInvoiceApplying},
		{"INV 例外审核", domain.InvFSM, domain.StateINVReadyForInvoice, domain.StateINVReceiptExReviewing},
		{"INV 例外通过", domain.InvFSM, domain.StateINVReceiptExReviewing, domain.StateINVInvoiceApplying},
		{"INV 例外拒绝待补回单", domain.InvFSM, domain.StateINVReceiptExReviewing, domain.StateINVWaitingReceipt},
		{"INV 开票完成", domain.InvFSM, domain.StateINVInvoicing, domain.StateINVInvoiced},
		{"INV 开票失败重试", domain.InvFSM, domain.StateINVInvoiceFailed, domain.StateINVInvoicing},
		{"INV 回款跟踪", domain.InvFSM, domain.StateINVInvoiced, domain.StateINVPaymentTracking},
		{"INV 关闭", domain.InvFSM, domain.StateINVContractArchived, domain.StateINVClosed},
		// PRE（§2.6）
		{"PRE 意图解析", domain.PreOrderFSM, domain.StatePreCaptured, domain.StatePreIntentParsing},
		{"PRE 待确认", domain.PreOrderFSM, domain.StatePreIntentParsing, domain.StatePrePendingConfirm},
		{"PRE 歧义", domain.PreOrderFSM, domain.StatePreIntentParsing, domain.StatePreAmbiguous},
		{"PRE 确认", domain.PreOrderFSM, domain.StatePrePendingConfirm, domain.StatePreConfirmed},
		{"PRE 修改后确认", domain.PreOrderFSM, domain.StatePrePendingConfirm, domain.StatePreModified},
		{"PRE 升级", domain.PreOrderFSM, domain.StatePrePendingConfirm, domain.StatePreEscalated},
		{"PRE 派生订单", domain.PreOrderFSM, domain.StatePreConfirmed, domain.StatePreOrderRequested},
	}
	for _, c := range edges {
		if err := c.fsm.Validate(c.from, c.to); err != nil {
			t.Errorf("%s: %s -> %s 应合法，实际被拒", c.name, c.from, c.to)
		}
	}

	// 非法迁移必须拒绝（抽样）
	illegal := []struct {
		fsm  *domain.FSM
		from string
		to   string
	}{
		{domain.OrdFSM, domain.StateORDRequested, domain.StateORDContracted}, // 未审价直达合同
		{domain.StkFSM, domain.StateSTKReadyForStock, domain.StateSTKPaymentConfirming},
		{domain.ShpFSM, domain.StateSHPShipAssigning, domain.StateINVClosed},
		{domain.InvFSM, domain.StateINVInvoiceApplying, domain.StateINVInvoiced}, // 未开票直接完成
		{domain.PreOrderFSM, domain.StatePreCaptured, domain.StatePreConfirmed},  // 未解析直接确认
	}
	for _, c := range illegal {
		if err := c.fsm.Validate(c.from, c.to); err == nil {
			t.Errorf("非法迁移未被拒绝: %s -> %s", c.from, c.to)
		}
	}
}

// ── 守恒断言契约（01 §1.2 constraints；CQ-06/07/08/09）──

func TestConservationContracts(t *testing.T) {
	// 备货 ≤ 订单
	if err := domain.AssertPrepareWithinOrder(90, 100); err != nil {
		t.Errorf("备货 90 ≤ 订单 100 应合法: %v", err)
	}
	if err := domain.AssertPrepareWithinOrder(101, 100); err == nil {
		t.Error("备货 101 > 订单 100 应拒绝")
	}
	// 发货 ≤ 备货
	if err := domain.AssertShipWithinPrepare(100, 100); err != nil {
		t.Errorf("发货 100 ≤ 备货 100 应合法: %v", err)
	}
	if err := domain.AssertShipWithinPrepare(101, 100); err == nil {
		t.Error("发货 101 > 备货 100 应拒绝")
	}
	// 完成条件：累计发货 = 订单量
	if !domain.ShipCompletion(100, 100) {
		t.Error("累计发货 = 订单量应判定全部发货")
	}
	if domain.ShipCompletion(99, 100) {
		t.Error("累计发货 < 订单量不应判定全部发货")
	}
	// 开票双维守恒：数量/金额任一超限拒绝
	if err := domain.AssertInvoiceWithinShipped(100, 100000, 100, 100000); err != nil {
		t.Errorf("开票=发货 应合法: %v", err)
	}
	if err := domain.AssertInvoiceWithinShipped(101, 100000, 100, 100000); err == nil {
		t.Error("开票数量超发货应拒绝")
	}
	if err := domain.AssertInvoiceWithinShipped(100, 100001, 100, 100000); err == nil {
		t.Error("开票金额超发货应拒绝")
	}
	// 库存恒非负
	if err := domain.AssertInventoryNonNeg(100, 100); err != nil {
		t.Errorf("库存 100 出 100 应合法: %v", err)
	}
	if err := domain.AssertInventoryNonNeg(100, 101); err == nil {
		t.Error("出库超库存应拒绝")
	}
}

// ── 幂等键规范契约（02 §5 键规范；CQ-11）──

func TestIdemKeyContracts(t *testing.T) {
	pre, err := domain.PreOrderKey("chang-an", "group-demo", "msg-1", "v1")
	if err != nil || pre != "pre:chang-an:group-demo:msg-1:v1" {
		t.Fatalf("PreOrderKey = %q, %v", pre, err)
	}
	if tenant, group, msg, ver, err := domain.ParsePreOrderKey(pre); err != nil || tenant != "chang-an" || group != "group-demo" || msg != "msg-1" || ver != "v1" {
		t.Fatalf("ParsePreOrderKey = %q %q %q %q, %v", tenant, group, msg, ver, err)
	}

	ord, err := domain.OrderKey("chang-an", "quick", "uuid-1")
	if err != nil || ord != "order:chang-an:quick:uuid-1" {
		t.Fatalf("OrderKey = %q, %v", ord, err)
	}
	if _, err := domain.OrderKey("chang-an", "bogus", "u"); err == nil {
		t.Error("非法 source 应拒绝")
	}

	seg, err := domain.SegmentKey(ord, "ship", 1)
	if err != nil || seg != "order:chang-an:quick:uuid-1:ship:1" {
		t.Fatalf("SegmentKey = %q, %v", seg, err)
	}
	if domain.RevertKey(ord) != "order:chang-an:quick:uuid-1:revert" {
		t.Error("RevertKey 派生错误")
	}
}

// ── 回单四分流契约（01 §2.5；CQ-10）──

func TestReceiptSplitContracts(t *testing.T) {
	cases := []struct {
		name string
		in   domain.ReceiptInput
		want domain.ReceiptSplit
	}{
		{"分流①款到直接进入", domain.ReceiptInput{Paid: true}, domain.SplitDirectInvoice},
		{"分流②有回单", domain.ReceiptInput{HasReceipt: true}, domain.SplitDirectInvoice},
		{"分流③无回单超5天", domain.ReceiptInput{DaysSinceShipment: 6}, domain.SplitReceiptException},
		{"分流④提前申请", domain.ReceiptInput{EarlyApply: true}, domain.SplitReceiptException},
		{"非款到无回单未超期→例外兜底", domain.ReceiptInput{}, domain.SplitReceiptException},
		{"有回单优先于超期", domain.ReceiptInput{HasReceipt: true, DaysSinceShipment: 9}, domain.SplitDirectInvoice},
	}
	for _, c := range cases {
		if got := domain.ClassifyReceipt(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// ── 接入层默认值契约（08 §4；Static 基线 = 历史硬编码，行为不变）──

func TestIntegrationPolicyContracts(t *testing.T) {
	p := integration.NewStaticConfigProvider()
	if p.RemindAfter() != 24*time.Hour || p.EscalateAfter() != 48*time.Hour {
		t.Errorf("审批催办默认值漂移: remind=%v escalate=%v", p.RemindAfter(), p.EscalateAfter())
	}
	if p.ReceiptExceptionAfter() != 5*24*time.Hour {
		t.Errorf("回单超期默认值漂移: %v", p.ReceiptExceptionAfter())
	}
	if p.PreOrderEscalateAfter() != 24*time.Hour {
		t.Errorf("预订单升级默认值漂移: %v", p.PreOrderEscalateAfter())
	}
	if p.AutoApprove("ORD", "A", 100000) {
		t.Error("基线全人工闸门：AutoApprove 应恒 false")
	}
	// 修改分类委托 domain 纯函数（A 优先于 B/C/D）
	if p.Classify(domain.Modification{CustomerNameChanged: true, PaymentTermChanged: true}) != domain.ModClassA {
		t.Error("Classify 应 A 优先")
	}
	if p.Classify(domain.Modification{PaymentTermChanged: true}) != domain.ModClassB {
		t.Error("Classify B 类判定漂移")
	}
	if p.Classify(domain.Modification{QuantityChanged: true}) != domain.ModClassD {
		t.Error("Classify D 类判定漂移")
	}
	if p.Classify(domain.Modification{}) != domain.ModClassE {
		t.Error("Classify E 类判定漂移")
	}
	if state, err := p.TargetState(domain.ModClassA); err != nil || state != domain.StateORDPriceReviewing {
		t.Errorf("TargetState(A) = %q, %v", state, err)
	}
}

// ── API 模型序列化契约（HTTP 端点直接 writeJSON 的类型；必须 snake_case）──

func TestAPIModelSnakeCaseContract(t *testing.T) {
	now := time.Now()
	task := repo.ApprovalTask{
		ID: "at-1", TenantID: "t1", OrderID: "ORD-1", StageCode: "PRICE_REVIEWING",
		Kind: "PAYMENT", State: "PENDING", AssigneeID: strPtr("u-1"),
		LegacyTaskID: strPtr("lt-1"), Deadline: &now, Verdict: nil, DecidedBy: nil,
		DecidedAt: nil, CreatedAt: now,
	}
	keys := jsonKeys(t, task)
	for _, want := range []string{"id", "tenant_id", "order_id", "stage_code", "kind", "state",
		"assignee_id", "legacy_task_id", "deadline", "created_at"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("ApprovalTask 缺 snake_case 键 %q（todos 端点契约）", want)
		}
	}
	// 未决字段必须省略（omitempty）
	for _, absent := range []string{"verdict", "decided_by", "decided_at"} {
		if _, ok := keys[absent]; ok {
			t.Errorf("ApprovalTask 未决字段 %q 不应出现在 JSON", absent)
		}
	}
	for k := range keys {
		if k != strings.ToLower(k) {
			t.Errorf("ApprovalTask 键 %q 非 snake_case（Go 默认字段名泄漏）", k)
		}
	}

	pre := repo.PreOrder{
		ID: "PRE-1", TenantID: "t1", GroupID: "g1", MsgID: "m1", State: "PRE_CAPTURED",
		Candidates: []byte(`[{"product_sku":"SKU-1"}]`), IdempotencyKey: "pre:t1:g1:m1:v1",
		CreatedAt: now, UpdatedAt: now,
	}
	keys = jsonKeys(t, pre)
	for _, want := range []string{"id", "tenant_id", "group_id", "msg_id", "state",
		"candidates", "idempotency_key", "created_at", "updated_at"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("PreOrder 缺 snake_case 键 %q（preorders 端点契约）", want)
		}
	}
	for k := range keys {
		if k != strings.ToLower(k) {
			t.Errorf("PreOrder 键 %q 非 snake_case（Go 默认字段名泄漏）", k)
		}
	}
}

func jsonKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	return m
}

func strPtr(s string) *string { return &s }

// ── 授权对照表契约（L0-09：StaticChecker 角色×动作矩阵）──

func TestStaticCheckerContract(t *testing.T) {
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) {
		return authz.Role(userID), nil
	})
	ok, err := checker.Check(t.Context(), authz.CheckRequest{
		Permission: authz.PermAppendEvidence, ResourceType: authz.ResShipment, SubjectID: string(authz.RoleWarehouse),
	})
	if err != nil || !ok {
		t.Errorf("仓管 append_evidence 应通过: %v %v", ok, err)
	}
	ok, _ = checker.Check(t.Context(), authz.CheckRequest{
		Permission: authz.PermAppendEvidence, ResourceType: authz.ResShipment, SubjectID: string(authz.RoleContact),
	})
	if ok {
		t.Error("客户联系人 append_evidence 应拒绝（场景 D 权限边界）")
	}
	ok, _ = checker.Check(t.Context(), authz.CheckRequest{
		Permission: authz.PermConfirm, ResourceType: authz.ResShipment, SubjectID: string(authz.RoleWarehouse),
	})
	if !ok {
		t.Error("仓管 confirm 应通过（场景 C）")
	}
	ok, _ = checker.Check(t.Context(), authz.CheckRequest{
		Permission: authz.PermVoid, ResourceType: authz.ResShipment, SubjectID: string(authz.RoleSales),
	})
	if ok {
		t.Error("销售 void 应拒绝")
	}
}
