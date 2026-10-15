package integration

import (
	"testing"
	"time"

	"b2b_pilot_v3/internal/domain"
)

// TestDemoPolicyDurations 演示策略虚拟时长契约（08 §5 本地变体；make cover 覆盖源）。
func TestDemoPolicyDurations(t *testing.T) {
	p := NewDemoPolicy()
	if p.RemindAfter() != DemoDuration || p.EscalateAfter() != DemoDuration ||
		p.ReceiptExceptionAfter() != DemoDuration || p.PreOrderEscalateAfter() != DemoDuration {
		t.Fatalf("Demo 策略时长漂移: remind=%v escalate=%v receipt=%v preorder=%v",
			p.RemindAfter(), p.EscalateAfter(), p.ReceiptExceptionAfter(), p.PreOrderEscalateAfter())
	}
	if DemoDuration <= 0 || DemoDuration >= time.Hour {
		t.Fatalf("DemoDuration 越界: %v", DemoDuration)
	}
}

// TestDemoPolicyAutoApprove 演示策略基线全人工闸门（与 Static 一致）。
func TestDemoPolicyAutoApprove(t *testing.T) {
	p := NewDemoPolicy()
	if p.AutoApprove("ORD", "A", 100000) {
		t.Error("Demo 基线 AutoApprove 应恒 false")
	}
	if p.AutoApprove("STK", "B", 0) {
		t.Error("Demo 基线 AutoApprove 应恒 false（任意入参）")
	}
}

// TestDemoPolicyClassifyDelegate 修改分类委托 domain 纯函数（与 Static 同一事实源）。
func TestDemoPolicyClassifyDelegate(t *testing.T) {
	p := NewDemoPolicy()
	cases := []struct {
		name string
		in   domain.Modification
		want domain.ModificationClass
	}{
		{"A 优先", domain.Modification{CustomerNameChanged: true, PaymentTermChanged: true}, domain.ModClassA},
		{"B 账期", domain.Modification{PaymentTermChanged: true}, domain.ModClassB},
		{"D 数量", domain.Modification{QuantityChanged: true}, domain.ModClassD},
		{"E 兜底", domain.Modification{}, domain.ModClassE},
	}
	for _, c := range cases {
		if got := p.Classify(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	if state, err := p.TargetState(domain.ModClassA); err != nil || state != domain.StateORDPriceReviewing {
		t.Errorf("TargetState(A) = %q, %v", state, err)
	}
	if state, err := p.TargetState(domain.ModClassB); err != nil || state != domain.StateSHPShipAssigning {
		t.Errorf("TargetState(B) = %q, %v", state, err)
	}
}
