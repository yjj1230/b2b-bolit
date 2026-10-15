package domain

import (
	"reflect"
	"testing"
)

// L0-01 · ORD FSM 全迁移穷举（合法+非法，含 A/B/C/D/E 修改分类）
// 倒推影响面：图谱 §2.2 状态图；CQ-04
func TestL001OrdFSMExhaustive(t *testing.T) {
	expected := map[string][]string{
		StateORDRequested:        {StateORDPriceReviewing, StateORDRejected},
		StateORDPriceReviewing:   {StateORDPriceApproved, StateORDReturned, StateORDRejected},
		StateORDPriceApproved:    {StateORDChairmanReview, StateORDContractGenerate},
		StateORDChairmanReview:   {StateORDContractGenerate, StateORDRejected, StateORDReturned},
		StateORDContractGenerate: {StateORDContracted, StateORDContractGenerate},
		StateORDContracted:       {StateSTKReadyForStock, StateORDModifying},
		StateORDModifying:        {StateORDPriceReviewing, StateSHPShipAssigning, StateORDContractGenerate, StateORDContracted},
		StateORDReturned:         {StateORDPriceReviewing, StateORDPriceApproved},
		StateORDRejected:         {},
	}
	assertFSMTable(t, OrdFSM, expected)
}

// L0-01 补充 · 修改分类规则表（A/B/C/D/E）与目标状态
func TestOrdModificationClassification(t *testing.T) {
	cases := []struct {
		name string
		m    Modification
		want ModificationClass
	}{
		{"A-customer-name", Modification{CustomerNameChanged: true}, ModClassA},
		{"A-product-price", Modification{ProductOrPriceChanged: true}, ModClassA},
		{"A-grade", Modification{GradeChanged: true}, ModClassA},
		{"A-beats-B", Modification{CustomerNameChanged: true, PaymentTermChanged: true}, ModClassA},
		{"A-beats-D", Modification{ProductOrPriceChanged: true, QuantityChanged: true}, ModClassA},
		{"B-payment-term", Modification{PaymentTermChanged: true}, ModClassB},
		{"B-beats-C", Modification{PaymentTermChanged: true, SupplierChannelChanged: true}, ModClassB},
		{"B-beats-D", Modification{PaymentTermChanged: true, AddressChanged: true}, ModClassB},
		{"C-supplier", Modification{SupplierChannelChanged: true}, ModClassC},
		{"D-address", Modification{AddressChanged: true}, ModClassD},
		{"D-product", Modification{ProductChanged: true}, ModClassD},
		{"D-quantity", Modification{QuantityChanged: true}, ModClassD},
		{"E-none", Modification{}, ModClassE},
		{"E-other", Modification{PaymentTermChanged: false, AddressChanged: false, QuantityChanged: false, ProductChanged: false}, ModClassE},
	}
	for _, c := range cases {
		if got := ClassifyModification(c.m); got != c.want {
			t.Errorf("%s: ClassifyModification = %s, want %s", c.name, got, c.want)
		}
	}

	// ModifyTargetState 与 FSM 迁移表一致
	targets := map[ModificationClass]string{
		ModClassA: StateORDPriceReviewing,
		ModClassB: StateSHPShipAssigning,
		ModClassC: StateORDContractGenerate,
		ModClassD: StateSHPShipAssigning,
		ModClassE: StateORDContracted,
	}
	for class, want := range targets {
		got, err := ModifyTargetState(class)
		if err != nil {
			t.Fatalf("ModifyTargetState(%s): %v", class, err)
		}
		if got != want {
			t.Errorf("ModifyTargetState(%s) = %s, want %s", class, got, want)
		}
		// 从 MODIFYING 出发的迁移必须合法
		if err := OrdFSM.Validate(StateORDModifying, want); err != nil {
			t.Errorf("ModifyTargetState(%s) -> %s is not a legal ORD transition: %v", class, want, err)
		}
	}
	if _, err := ModifyTargetState("X"); err == nil {
		t.Error("ModifyTargetState(X) should error")
	}
}

// L0-02 · STK FSM 穷举（款到/非款到/转仓/退回）
func TestL002StkFSMExhaustive(t *testing.T) {
	expected := map[string][]string{
		StateSTKReadyForStock:      {StateSTKStockReviewing, StateSHPShipAssigning},
		StateSTKStockReviewing:     {StateSTKWarehouseConfirm, StateSTKReturned}, // 0.1：商务备货驳回 → 退回修改
		StateSTKWarehouseConfirm:   {StateSTKWarehouseConfirmed, StateSTKWarehouseReturned, StateSTKTransferRequested},
		StateSTKWarehouseReturned:  {StateSTKWarehouseConfirm, StateSTKStockReviewing},
		StateSTKTransferRequested:  {StateSTKWarehouseConfirm, StateSTKWarehouseConfirmed, StateSTKReturned},
		StateSTKWarehouseConfirmed: {StateSTKPaymentConfirming, StateSTKShipOrderIssued},
		StateSTKPaymentConfirming:  {StateSTKPaymentConfirmed, StateSTKReturned},
		StateSTKPaymentConfirmed:   {StateSTKShipOrderIssued},
		StateSTKShipOrderIssued:    {StateSHPShipAssigning},
		StateSTKReturned:           {StateSTKStockReviewing},
	}
	assertFSMTable(t, StkFSM, expected)
}

// L0-03 · SHP FSM 穷举（分批/退回/转仓/完成条件）
func TestL003ShpFSMExhaustive(t *testing.T) {
	expected := map[string][]string{
		// 实施校准（05 §3）：SHIP_ASSIGNING → SHIPPED 直连边（信号直达单事务实发）
		StateSHPShipAssigning:  {StateSHPWarehouseShip, StateSHPShipPed, StateSTKWarehouseConfirm},
		StateSHPWarehouseShip:  {StateSHPShipPed, StateSHPShipAssigning},
		StateSHPShipPed:        {StateSHPFullyShipped, StateSHPPartialShipped},
		StateSHPPartialShipped: {StateSHPShipAssigning, StateSTKWarehouseConfirm, StateINVReadyForInvoice},
		StateSHPFullyShipped:   {StateINVReadyForInvoice},
	}
	assertFSMTable(t, ShpFSM, expected)
}

// L0-03 补充 · 发货完成条件（CQ-07）：累计发货 = 订单量
func TestShipCompletion(t *testing.T) {
	cases := []struct {
		shipped, order int64
		want           bool
	}{
		{0, 100, false},
		{99, 100, false},
		{100, 100, true},
		{101, 100, false}, // 超发不可能出现（守恒校验前置），判定仍为未完成
	}
	for _, c := range cases {
		if got := ShipCompletion(c.shipped, c.order); got != c.want {
			t.Errorf("ShipCompletion(%d,%d) = %v, want %v", c.shipped, c.order, got, c.want)
		}
	}
}

// L0-04 · INV FSM 穷举（四分流/开票状态/例外）
func TestL004InvFSMExhaustive(t *testing.T) {
	expected := map[string][]string{
		StateINVReadyForInvoice:    {StateINVInvoiceApplying, StateINVReceiptExReviewing},
		StateINVReceiptExReviewing: {StateINVInvoiceApplying, StateINVWaitingReceipt},
		StateINVWaitingReceipt:     {StateINVInvoiceApplying},
		StateINVInvoiceApplying:    {StateINVInvoicing},
		StateINVInvoicing:          {StateINVInvoiced, StateINVInvoiceFailed, StateINVNoInvoiceNeeded},
		StateINVInvoiced:           {StateINVPaymentTracking},
		StateINVNoInvoiceNeeded:    {StateINVPaymentTracking},
		StateINVInvoiceFailed:      {StateINVInvoicing},
		StateINVPaymentTracking:    {StateINVContractArchived},
		StateINVContractArchived:   {StateINVClosed},
		StateINVClosed:             {},
	}
	assertFSMTable(t, InvFSM, expected)
}

// L0-05 · PreOrder FSM 穷举（确认/修改/忽略/升级）
func TestL005PreOrderFSMExhaustive(t *testing.T) {
	expected := map[string][]string{
		StatePreCaptured:       {StatePreCollecting, StatePreIntentParsing},
		StatePreIntentParsing:  {StatePreCollecting, StatePrePendingConfirm, StatePreAmbiguous, StatePreIgnored},
		StatePreCollecting:     {StatePreCollecting, StatePrePendingConfirm, StatePreAmbiguous, StatePreEscalated, StatePreExpired, StatePreIgnored},
		StatePreAmbiguous:      {StatePreCollecting, StatePrePendingConfirm, StatePreIgnored, StatePreExpired},
		StatePrePendingConfirm: {StatePreCollecting, StatePreConfirmed, StatePreModified, StatePreIgnored, StatePreEscalated, StatePreExpired},
		StatePreConfirmed:      {StatePreOrderRequested},
		StatePreModified:       {StatePreOrderRequested},
		StatePreEscalated:      {StatePreCollecting, StatePrePendingConfirm, StatePreExpired},
		StatePreExpired:        {},
		StatePreIgnored:        {},
	}
	assertFSMTable(t, PreOrderFSM, expected)
}

// L0-04/05 补充 · 终态判定
func TestIsTerminal(t *testing.T) {
	for s, want := range map[string]bool{
		StateORDRejected:   true,
		StatePreIgnored:    true,
		StateINVClosed:     true,
		StateORDContracted: false,
		StateINVInvoiced:   false,
		"BOGUS":            false,
	} {
		if got := IsTerminal(s); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, want)
		}
	}
}

// L0-01..05 补充 · AI 安全边界（§2.6）：未确认不得生成正式订单
func TestPreOrderHumanGate(t *testing.T) {
	// CONFIRMED/MODIFIED 是进入 ORDER_REQUESTED 的唯一入口
	for _, from := range PreOrderFSM.Next(StatePreIntentParsing) {
		if from == StatePreAmbiguous {
			if PreOrderFSM.Can(from, StatePreOrderRequested) {
				t.Error("AMBIGUOUS must not auto-create order (AI safety boundary)")
			}
		}
	}
	if PreOrderFSM.Can(StatePreCaptured, StatePreOrderRequested) {
		t.Error("CAPTURED must not jump to ORDER_REQUESTED (human gate)")
	}
	if PreOrderFSM.Can(StatePrePendingConfirm, StatePreOrderRequested) {
		t.Error("PENDING_CONFIRM must not jump to ORDER_REQUESTED (human gate)")
	}
}

// assertFSMTable 穷举：期望迁移必须合法、其余全部非法、且 Next 与期望一致。
func assertFSMTable(t *testing.T, fsm *FSM, expected map[string][]string) {
	t.Helper()
	states := fsm.States()
	for _, from := range states {
		for _, to := range states {
			wantLegal := contains(expected[from], to)
			gotLegal := fsm.Can(from, to)
			if gotLegal != wantLegal {
				t.Errorf("%s: %s -> %s legal = %v, want %v", fsm.name, from, to, gotLegal, wantLegal)
			}
			err := fsm.Validate(from, to)
			if wantLegal && err != nil {
				t.Errorf("%s: %s -> %s should be legal but Validate errored: %v", fsm.name, from, to, err)
			}
			if !wantLegal && err == nil {
				t.Errorf("%s: %s -> %s should be illegal", fsm.name, from, to)
			}
			if !wantLegal && !IsIllegalTransition(err) {
				t.Errorf("%s: %s -> %s error should be ErrIllegalTransition, got %T", fsm.name, from, to, err)
			}
		}
		// Next 返回集合与期望一致（无序比较）
		if !sameSet(fsm.Next(from), expected[from]) {
			t.Errorf("%s: Next(%s) = %v, want %v", fsm.name, from, fsm.Next(from), expected[from])
		}
	}
	// 迁移表不允许出现未声明状态的目标
	for from, tos := range expected {
		for _, to := range tos {
			if !contains(states, to) {
				t.Errorf("%s: expected table references undeclared state %q (from %s)", fsm.name, to, from)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ma := map[string]bool{}
	for _, v := range a {
		ma[v] = true
	}
	for _, v := range b {
		if !ma[v] {
			return false
		}
	}
	return true
}

// FSM 通用行为
func TestFSMGeneral(t *testing.T) {
	fsm := NewFSM("T", "S0", map[string][]string{"S0": {"S1"}, "S1": {}})
	if fsm.Entry() != "S0" {
		t.Error("Entry mismatch")
	}
	if !fsm.Can("S0", "S1") || fsm.Can("S1", "S0") {
		t.Error("Can mismatch")
	}
	if err := fsm.Validate("S1", "S0"); err == nil || !IsIllegalTransition(err) {
		t.Error("Validate should return ErrIllegalTransition")
	}
	if err := fsm.Validate("S0", "S1"); err != nil {
		t.Errorf("Validate legal transition failed: %v", err)
	}
	got := fsm.States()
	want := []string{"S0", "S1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("States = %v, want %v", got, want)
	}
	if got := fsm.Next("S0"); !reflect.DeepEqual(got, []string{"S1"}) {
		t.Errorf("Next = %v", got)
	}
	// ErrIllegalTransition 错误类型（fsm.go 分支补全）
	if (&ErrIllegalTransition{FSM: "T", From: "A", To: "B"}).Error() == "" {
		t.Error("ErrIllegalTransition.Error should produce a message")
	}
	if Stage("ORD_X") != "ORD" || Stage(StatePreCaptured) != "PRE" || Stage("AB") != "" {
		t.Error("Stage parsing mismatch")
	}
}

// D-03：已发货订单禁止直接改收货地址（BLOCK → 物流异常），未发货/其他分类不拦。
func TestBlockShippedAddressChange(t *testing.T) {
	addrChanges := []string{"receive_address: 湖南岳阳→新地址"}
	if !BlockShippedAddressChange(StateSHPShipPed, ModClassD, addrChanges) {
		t.Error("shipped order address change must be blocked")
	}
	if !BlockShippedAddressChange(StateSHPFullyShipped, ModClassD, []string{"地址变更"}) {
		t.Error("fully shipped address change must be blocked")
	}
	if !BlockShippedAddressChange(StateINVInvoicing, ModClassD, addrChanges) {
		t.Error("invoicing address change must be blocked")
	}
	if BlockShippedAddressChange(StateORDPriceReviewing, ModClassD, addrChanges) {
		t.Error("pre-ship address change must be allowed (D 类直达发货)")
	}
	if BlockShippedAddressChange(StateSHPShipPed, ModClassA, addrChanges) {
		t.Error("non-D class must not be blocked here")
	}
	if BlockShippedAddressChange(StateSHPShipPed, ModClassD, []string{"quantity: 2"}) {
		t.Error("non-address D change must not be blocked here")
	}
}
