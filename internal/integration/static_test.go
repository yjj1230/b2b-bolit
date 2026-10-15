package integration

import (
	"testing"
	"time"

	"b2b_pilot_v3/internal/domain"
)

// 基线默认值 = 历史硬编码常量（行为不变）验证。
func TestStaticConfigProviderDefaults(t *testing.T) {
	p := NewStaticConfigProvider()

	if got := p.RemindAfter(); got != 24*time.Hour {
		t.Errorf("RemindAfter = %v, want 24h", got)
	}
	if got := p.EscalateAfter(); got != 48*time.Hour {
		t.Errorf("EscalateAfter = %v, want 48h", got)
	}
	if got := p.ReceiptExceptionAfter(); got != 5*24*time.Hour {
		t.Errorf("ReceiptExceptionAfter = %v, want 5d", got)
	}
	if got := p.PreOrderEscalateAfter(); got != 24*time.Hour {
		t.Errorf("PreOrderEscalateAfter = %v, want 24h", got)
	}
}

// 修改分类委托 domain 纯函数：规则一致性验证（A 优先于 B/C/D）。
func TestStaticConfigProviderModificationRule(t *testing.T) {
	p := NewStaticConfigProvider()

	cases := []struct {
		name string
		m    domain.Modification
		want domain.ModificationClass
	}{
		{"A-grade", domain.Modification{GradeChanged: true}, domain.ModClassA},
		{"A-beats-B", domain.Modification{CustomerNameChanged: true, PaymentTermChanged: true}, domain.ModClassA},
		{"B-payment-term", domain.Modification{PaymentTermChanged: true}, domain.ModClassB},
		{"B-beats-C", domain.Modification{PaymentTermChanged: true, SupplierChannelChanged: true}, domain.ModClassB},
		{"C-supplier", domain.Modification{SupplierChannelChanged: true}, domain.ModClassC},
		{"D-address", domain.Modification{AddressChanged: true}, domain.ModClassD},
		{"E-other", domain.Modification{}, domain.ModClassE},
	}
	for _, c := range cases {
		if got := p.Classify(c.m); got != c.want {
			t.Errorf("%s: Classify = %s, want %s", c.name, got, c.want)
		}
	}

	// TargetState 与 OrdFSM 迁移表一致（A → 回审价；B/D → 直达发货；C → 重生成合同；E → 静默）。
	if got, err := p.TargetState(domain.ModClassA); err != nil || got != domain.StateORDPriceReviewing {
		t.Errorf("TargetState(A) = %q, %v; want %q", got, err, domain.StateORDPriceReviewing)
	}
	if got, err := p.TargetState(domain.ModClassC); err != nil || got != domain.StateORDContractGenerate {
		t.Errorf("TargetState(C) = %q, %v; want %q", got, err, domain.StateORDContractGenerate)
	}
	if _, err := p.TargetState("X"); err == nil {
		t.Error("TargetState(X) should error")
	}
}

// 基线无自动审批（全部人工闸门，01 §2.1）。
func TestStaticConfigProviderAutoApproveDisabled(t *testing.T) {
	p := NewStaticConfigProvider()
	if p.AutoApprove("ORD_PRICE", "C", 1_000_000_00) {
		t.Error("baseline AutoApprove must be false (all human gates)")
	}
}

// 动态数据源注册表。
func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeSource{name: "wecom"})
	r.Register(fakeSource{name: "feishu_bitable"})

	if got := r.Get("wecom").SourceName(); got != "wecom" {
		t.Errorf("Get(wecom) = %v", got)
	}
	if r.Get("missing") != nil {
		t.Error("Get(missing) should be nil")
	}
}

type fakeSource struct{ name string }

func (f fakeSource) SourceName() string { return f.name }

// 编译期断言接口完整实现。
var _ BusinessConfigProvider = (*StaticConfigProvider)(nil)
