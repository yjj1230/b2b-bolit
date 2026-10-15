package domain

import "testing"

// L0-06 · 数量/金额值对象（零/负/溢出拒绝）
func TestL006ValueObjects(t *testing.T) {
	// 正数量
	if _, err := NewQty(0); err == nil {
		t.Error("NewQty(0) should fail")
	}
	if _, err := NewQty(-5); err == nil {
		t.Error("NewQty(-5) should fail")
	}
	q, err := NewQty(10)
	if err != nil {
		t.Fatalf("NewQty(10): %v", err)
	}
	if q.Int64() != 10 {
		t.Error("Qty.Int64 mismatch")
	}
	if (&ErrInvalidQuantity{V: 0}).Error() == "" {
		t.Error("ErrInvalidQuantity should produce a message")
	}

	// Add 溢出拒绝
	big, _ := NewQty(1 << 62)
	if _, err := big.Add(big); err == nil {
		t.Error("Add overflow should fail")
	}
	s, err := NewQtyMust(5).Add(NewQtyMust(7))
	if err != nil || s.Int64() != 12 {
		t.Errorf("Add = %d, %v", s.Int64(), err)
	}
	// Sub 结果必须 > 0
	if _, err := NewQtyMust(5).Sub(NewQtyMust(5)); err == nil {
		t.Error("Sub to zero should fail (order qty must stay positive)")
	}
	if _, err := NewQtyMust(5).Sub(NewQtyMust(6)); err == nil {
		t.Error("Sub to negative should fail")
	}
	if _, err := NewQtyMust(5).Sub(NewQtyMust(2)); err != nil {
		t.Errorf("Sub(2): %v", err)
	}

	// 非负数量（库存）
	if _, err := NewNonNegQty(-1); err == nil {
		t.Error("NewNonNegQty(-1) should fail")
	}
	z, err := NewNonNegQty(0)
	if err != nil {
		t.Fatalf("NewNonNegQty(0): %v", err)
	}
	if z.Int64() != 0 {
		t.Error("NonNegQty.Int64 mismatch")
	}
	if _, err := NewNonNegQtyMust(3).Sub(NewNonNegQtyMust(5)); err == nil {
		t.Error("NonNeg Sub to negative should fail")
	}
	r, err := NewNonNegQtyMust(5).Sub(NewNonNegQtyMust(5))
	if err != nil || r.Int64() != 0 {
		t.Errorf("NonNeg Sub(5) = %d, %v", r.Int64(), err)
	}
	// ErrOverflow（qty 域）错误消息
	if (&ErrOverflow{Op: "qty sub"}).Error() == "" {
		t.Error("ErrOverflow.Error should produce a message")
	}
}

// L0-06 · 金额值对象
func TestAmountCents(t *testing.T) {
	if _, err := NewAmountCents(-1); err == nil {
		t.Error("NewAmountCents(-1) should fail")
	}
	a, err := NewAmountCents(100)
	if err != nil {
		t.Fatalf("NewAmountCents(100): %v", err)
	}
	if a.Int64() != 100 {
		t.Error("AmountCents.Int64 mismatch")
	}
	sum, err := a.Add(NewAmountCentsMust(50))
	if err != nil || sum.Int64() != 150 {
		t.Errorf("Add = %d, %v", sum.Int64(), err)
	}
	if _, err := NewAmountCentsMust(5).Sub(NewAmountCentsMust(6)); err == nil {
		t.Error("Sub to negative should fail")
	}
	diff, err := NewAmountCentsMust(5).Sub(NewAmountCentsMust(5))
	if err != nil || diff.Int64() != 0 {
		t.Errorf("Sub(5) = %d, %v", diff.Int64(), err)
	}
	// 溢出拒绝
	big, _ := NewAmountCents(1 << 62)
	if _, err := big.Add(big); err == nil {
		t.Error("Amount Add overflow should fail")
	}
	// 金额 × 数量
	prod, err := NewAmountCentsMust(100).MulQty(NewQtyMust(3))
	if err != nil || prod.Int64() != 300 {
		t.Errorf("MulQty = %d, %v", prod.Int64(), err)
	}
	if _, err := NewAmountCentsMust(1 << 62).MulQty(NewQtyMust(2)); err == nil {
		t.Error("MulQty overflow should fail")
	}
	// 错误类型
	if _, ok := interface{}(&ErrInvalidAmount{V: 1}).(error); !ok {
		t.Error("ErrInvalidAmount should implement error")
	}
	if _, ok := interface{}(&ErrOverflow{Op: "x"}).(error); !ok {
		t.Error("ErrOverflow should implement error")
	}
	if (&ErrInvalidAmount{V: -1}).Error() == "" {
		t.Error("ErrInvalidAmount.Error should produce a message")
	}
}

// L0-07 · 守恒断言纯函数（备货≤订单、发货≤备货、开票≤发货、库存≥0）
func TestL007ConsistencyAssertions(t *testing.T) {
	// 备货守恒
	if err := AssertPrepareWithinOrder(0, 100); err != nil {
		t.Errorf("prepare 0<=100: %v", err)
	}
	if err := AssertPrepareWithinOrder(100, 100); err != nil {
		t.Errorf("prepare 100<=100: %v", err)
	}
	if err := AssertPrepareWithinOrder(101, 100); err == nil || !IsConservation(err) {
		t.Error("prepare 101>100 should be conservation violation")
	}
	if err := AssertPrepareWithinOrder(-1, 100); err == nil {
		t.Error("negative input should fail")
	}

	// 发货守恒
	if err := AssertShipWithinPrepare(100, 100); err != nil {
		t.Errorf("ship 100<=100: %v", err)
	}
	if err := AssertShipWithinPrepare(101, 100); err == nil {
		t.Error("ship 101>100 should be conservation violation")
	}
	if err := AssertShipWithinPrepare(-1, 100); err == nil {
		t.Error("negative shipped input should fail")
	}
	if err := AssertShipWithinPrepare(100, -1); err == nil {
		t.Error("negative prepared input should fail")
	}

	// 开票守恒（金额+数量双维度，任一超限拒绝）
	if err := AssertInvoiceWithinShipped(50, 5000, 100, 10000); err != nil {
		t.Errorf("invoice within: %v", err)
	}
	if err := AssertInvoiceWithinShipped(100, 10000, 100, 10000); err != nil {
		t.Errorf("invoice equal: %v", err)
	}
	if err := AssertInvoiceWithinShipped(101, 5000, 100, 10000); err == nil {
		t.Error("invoice qty over shipped qty should fail")
	}
	if err := AssertInvoiceWithinShipped(50, 10001, 100, 10000); err == nil {
		t.Error("invoice amount over shipped amount should fail")
	}
	if err := AssertInvoiceWithinShipped(-1, 0, 100, 10000); err == nil {
		t.Error("negative invoice qty should fail")
	}

	// ErrConservation 错误类型
	if (&ErrConservation{Kind: "x", Msg: "y"}).Error() == "" {
		t.Error("ErrConservation.Error should produce a message")
	}
	if err := AssertInventoryNonNeg(5, 5); err != nil {
		t.Errorf("inventory 5-5: %v", err)
	}
	if err := AssertInventoryNonNeg(4, 5); err == nil {
		t.Error("inventory 4-5 should fail")
	}
	if err := AssertInventoryNonNeg(-1, 0); err == nil {
		t.Error("negative inventory should fail")
	}
}

// L0-10 · 回单四分流判定（款到/有回单/超 5 天/提前申请）
func TestL010ReceiptSplit(t *testing.T) {
	cases := []struct {
		name string
		in   ReceiptInput
		want ReceiptSplit
	}{
		{"分流①款到直接进入", ReceiptInput{Paid: true}, SplitDirectInvoice},
		{"分流②有回单", ReceiptInput{HasReceipt: true}, SplitDirectInvoice},
		{"分流③无回单超5天", ReceiptInput{DaysSinceShipment: 6}, SplitReceiptException},
		{"分流③恰好5天不算超", ReceiptInput{DaysSinceShipment: 5}, SplitReceiptException}, // 无回单且未款到 → 例外入口
		{"分流④提前申请", ReceiptInput{EarlyApply: true, DaysSinceShipment: 1}, SplitReceiptException},
		{"款到且超5天 → 例外优先", ReceiptInput{Paid: true, DaysSinceShipment: 6}, SplitReceiptException},
		{"有回单 → 直接开票（有回单不触发例外）", ReceiptInput{HasReceipt: true, DaysSinceShipment: 6}, SplitDirectInvoice},
	}
	for _, c := range cases {
		if got := ClassifyReceipt(c.in); got != c.want {
			t.Errorf("%s: ClassifyReceipt = %s, want %s", c.name, got, c.want)
		}
	}
}

// L0-08 · 幂等键规范（构造/解析/校验，预订单/订单/发货/开票）
func TestL008IdempotencyKeys(t *testing.T) {
	// 预订单键
	pre, err := PreOrderKey("t1", "g1", "m1", "v1")
	if err != nil || pre != "pre:t1:g1:m1:v1" {
		t.Fatalf("PreOrderKey = %q, %v", pre, err)
	}
	ten, grp, msg, ver, err := ParsePreOrderKey(pre)
	if err != nil || ten != "t1" || grp != "g1" || msg != "m1" || ver != "v1" {
		t.Errorf("ParsePreOrderKey = %q %q %q %q %v", ten, grp, msg, ver, err)
	}
	if _, err := PreOrderKey("t1", "", "m1", "v1"); err == nil {
		t.Error("empty group should fail")
	}
	if _, err := PreOrderKey("t1", "g1", "m1", "v1"); err != nil {
		t.Errorf("re-construct: %v", err)
	}
	if _, _, _, _, err := ParsePreOrderKey("order:t1:quick:u1"); err == nil {
		t.Error("parse order key as pre key should fail")
	}
	if _, _, _, _, err := ParsePreOrderKey("pre:a:b:c"); err == nil {
		t.Error("pre key with 4 parts should fail")
	}

	// 订单键
	ok, err := OrderKey("t1", "quick", "u1")
	if err != nil || ok != "order:t1:quick:u1" {
		t.Fatalf("OrderKey = %q, %v", ok, err)
	}
	if _, err := OrderKey("t1", "pre", "u1"); err != nil {
		t.Errorf("OrderKey(pre): %v", err)
	}
	if _, err := OrderKey("t1", "bogus", "u1"); err == nil {
		t.Error("OrderKey with bad source should fail")
	}
	if _, err := OrderKey("", "quick", "u1"); err == nil {
		t.Error("empty tenant should fail")
	}
	ten, src, uuid, err := ParseOrderKey(ok)
	if err != nil || ten != "t1" || src != "quick" || uuid != "u1" {
		t.Errorf("ParseOrderKey = %q %q %q %v", ten, src, uuid, err)
	}
	if _, _, _, err := ParseOrderKey("pre:t1:g1:m1:v1"); err == nil {
		t.Error("parse pre key as order key should fail")
	}
	if _, _, _, err := ParseOrderKey("order:a:b:c:d"); err == nil {
		t.Error("order key with 5 parts should fail")
	}

	// 分段键（发货/开票）
	shipKey, err := SegmentKey(ok, "ship", 1)
	if err != nil || shipKey != "order:t1:quick:u1:ship:1" {
		t.Fatalf("SegmentKey(ship) = %q, %v", shipKey, err)
	}
	invKey, err := SegmentKey(ok, "inv", 2)
	if err != nil || invKey != "order:t1:quick:u1:inv:2" {
		t.Fatalf("SegmentKey(inv) = %q, %v", invKey, err)
	}
	if _, err := SegmentKey("pre:x", "ship", 1); err == nil {
		t.Error("SegmentKey on non-order key should fail")
	}
	if _, err := SegmentKey(ok, "bogus", 1); err == nil {
		t.Error("SegmentKey with bad segment should fail")
	}

	// 补偿键
	if RevertKey(shipKey) != shipKey+":revert" {
		t.Error("RevertKey mismatch")
	}

	// 错误类型与分支补全
	if (&ErrInvalidIdemKey{Key: "k", Reason: "r"}).Error() == "" {
		t.Error("ErrInvalidIdemKey.Error should produce a message")
	}
	if _, _, _, _, err := ParsePreOrderKey("pre"); err == nil {
		t.Error("single-part pre key should fail (too few parts)")
	}
	if _, _, _, err := ParseOrderKey("order"); err == nil {
		t.Error("single-part order key should fail (too few parts)")
	}
	if _, _, _, err := ParseOrderKey("order::quick:u1"); err == nil {
		t.Error("order key with empty tenant should fail")
	}
	if _, err := SegmentKey("order:x", "ship", 0); err != nil {
		t.Errorf("SegmentKey seq 0 should be allowed: %v", err)
	}
}

// helpers —— 测试用便捷构造（must 版本）
func NewQtyMust(v int64) Qty {
	q, err := NewQty(v)
	if err != nil {
		panic(err)
	}
	return q
}

func NewAmountCentsMust(v int64) AmountCents {
	a, err := NewAmountCents(v)
	if err != nil {
		panic(err)
	}
	return a
}

func NewNonNegQtyMust(v int64) NonNegQty {
	n, err := NewNonNegQty(v)
	if err != nil {
		panic(err)
	}
	return n
}
