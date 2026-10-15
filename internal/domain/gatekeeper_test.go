package domain

import (
	"strings"
	"testing"
)

// ── L0 单元：发群守门员纯函数域（docs/v0.1/09-testing N-01；domain 100% 硬门禁）──

func TestGatekeeperFSMTransitions(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
		ok   bool
	}{
		{"pending→confirmed", GatekeeperPending, GatekeeperConfirmed, true},
		{"pending→cancelled", GatekeeperPending, GatekeeperCancelled, true},
		{"confirmed→confirmed(重复确认)", GatekeeperConfirmed, GatekeeperConfirmed, false},
		{"confirmed→cancelled", GatekeeperConfirmed, GatekeeperCancelled, false},
		{"cancelled→confirmed", GatekeeperCancelled, GatekeeperConfirmed, false},
		{"cancelled→cancelled", GatekeeperCancelled, GatekeeperCancelled, false},
		{"非法状态→confirmed", "UNKNOWN", GatekeeperConfirmed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := GatekeeperFSM.Validate(c.from, c.to)
			if c.ok && err != nil {
				t.Fatalf("expected legal %s→%s, got %v", c.from, c.to, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected illegal %s→%s, got nil", c.from, c.to)
			}
			if !c.ok && !IsIllegalTransition(err) {
				t.Fatalf("expected ErrIllegalTransition, got %v", err)
			}
		})
	}
}

func TestGatekeeperContentHashNormalization(t *testing.T) {
	// 空白差异不改变指纹（同一内容重复确认命中既有记录）
	a := GatekeeperContentHash("  订单卡片 ORD-2026-0112 " + "\n\t" + " 已发货 ")
	b := GatekeeperContentHash("订单卡片 ORD-2026-0112 已发货")
	if a != b {
		t.Fatalf("normalization broke hash equality: %s != %s", a, b)
	}
	if a == "" || len(a) != 32 {
		t.Fatalf("unexpected hash %q (len=%d, want 32)", a, len(a))
	}
	// 不同内容不同指纹
	if GatekeeperContentHash("订单卡片 A") == GatekeeperContentHash("订单卡片 B") {
		t.Fatal("different content collided")
	}
}

func TestGatekeeperKey(t *testing.T) {
	k := GatekeeperKey("chang-an", "ORD-1", "abcd1234")
	if k != "gk:chang-an:ORD-1:abcd1234" {
		t.Fatalf("unexpected key %q", k)
	}
}

func TestValidateGatekeeperContent(t *testing.T) {
	if err := ValidateGatekeeperContent("  "); err == nil {
		t.Fatal("empty content should fail")
	}
	if err := ValidateGatekeeperContent("正常内容"); err != nil {
		t.Fatalf("normal content: %v", err)
	}
	if err := ValidateGatekeeperContent(strings.Repeat("x", 8193)); err == nil {
		t.Fatal("oversized content should fail")
	}
	if err := ValidateGatekeeperContent(strings.Repeat("x", 8192)); err != nil {
		t.Fatalf("8192 bytes should pass: %v", err)
	}
}

func TestConfirmGatekeeper(t *testing.T) {
	if err := ConfirmGatekeeper(GatekeeperPending); err != nil {
		t.Fatalf("pending confirm: %v", err)
	}
	if err := ConfirmGatekeeper(GatekeeperConfirmed); err != ErrGatekeeperAlreadyConfirmed {
		t.Fatalf("duplicate confirm: want ErrGatekeeperAlreadyConfirmed, got %v", err)
	}
	if err := ConfirmGatekeeper(GatekeeperCancelled); err == nil {
		t.Fatal("cancelled confirm should be illegal")
	}
}

func TestCancelGatekeeper(t *testing.T) {
	if err := CancelGatekeeper(GatekeeperPending); err != nil {
		t.Fatalf("pending cancel: %v", err)
	}
	if err := CancelGatekeeper(GatekeeperConfirmed); err == nil {
		t.Fatal("confirmed cancel should be illegal")
	}
}

func TestValidateStockReviewVerdict(t *testing.T) {
	for _, v := range []string{"APPROVED", "RETURNED"} {
		if !ValidateStockReviewVerdict(v) {
			t.Fatalf("verdict %s should be valid", v)
		}
	}
	for _, v := range []string{"", "REJECTED", "approved", "SHIP"} {
		if ValidateStockReviewVerdict(v) {
			t.Fatalf("verdict %q should be invalid", v)
		}
	}
}

func TestValidateWarehouseSelection(t *testing.T) {
	if err := ValidateWarehouseSelection("APPROVED", "  "); err == nil {
		t.Fatal("APPROVED without warehouse should fail")
	}
	if err := ValidateWarehouseSelection("APPROVED", "wh-yangyuan"); err != nil {
		t.Fatalf("APPROVED with warehouse: %v", err)
	}
	if err := ValidateWarehouseSelection("RETURNED", ""); err != nil {
		t.Fatalf("RETURNED without warehouse: %v", err)
	}
}

func TestInvoiceLinkKey(t *testing.T) {
	k := InvoiceLinkKey("chang-an", "INV-1", "ORD-2")
	if k != "invlink:chang-an:INV-1:ORD-2" {
		t.Fatalf("unexpected key %q", k)
	}
}
