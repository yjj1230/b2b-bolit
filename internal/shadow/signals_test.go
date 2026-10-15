package shadow

import (
	"testing"

	"b2b_pilot_v3/internal/workflows"
)

// S-05 · 权威映射配置自检：config/shadow-feed.example.json 的每条规则都能翻译，
// 且翻译产物能收敛为 workflow 强类型 Signal（配置漂移/字段缺失在发布前即暴露）。
func TestS05ExampleFeedConfigSignalsRoundTrip(t *testing.T) {
	cfg, err := LoadFeedConfig("../../config/shadow-feed.example.json")
	if err != nil {
		t.Fatalf("example feed config invalid: %v", err)
	}
	payloads := map[string]map[string]any{
		"创建订单":      {"customer_id": "cust-1", "payment_term": "款到", "quantity": 3, "unit_price_cents": 3200},
		"主管通过":      {"operator": "u-supervisor", "reason": "ok"},
		"董事长通过":     {"operator": "u-chairman"},
		"商务备货通过":    {"operator": "u-op", "warehouse_id": "wh-yangyuan"},
		"仓管确认":      {"operator": "u-wh-yangyuan"},
		"财务确认到款":    {"operator": "u-accounting", "ledger_ref": "L1"},
		"仓管确认发货":    {"operator": "u-wh-yangyuan", "qty": 3},
		"无回单超期进入例外": {"operator": "system"},
		"例外审核放行":    {"operator": "u-op", "deadline_days": 3},
		"财务开票完成":    {"operator": "u-accounting", "tax_no": "T-1"},
		"回款登记":      {"operator": "u-accounting", "amount": 9600},
	}
	for _, rule := range cfg.Rules {
		p, ok := payloads[rule.Action]
		if !ok {
			continue // 配置覆盖比最小自检集合广；未列动作由 Translate 单测覆盖
		}
		ev := SourceEvent{TenantID: cfg.TenantID, StageCode: rule.StageCode, Action: rule.Action, Payload: p}
		d, err := Translate(cfg, ev)
		if err != nil {
			t.Fatalf("rule %q/%q failed translate: %v", rule.StageCode, rule.Action, err)
		}
		if d.Start {
			continue // 建单规则不产生 Signal
		}
		if _, err := TypedSignal(d.Signal, d.Payload); err != nil {
			t.Fatalf("rule %q/%q produced untyped-able signal %q: %v", rule.StageCode, rule.Action, d.Signal, err)
		}
	}
}

func TestTypedSignalMatrix(t *testing.T) {
	cases := []struct {
		signal  string
		payload map[string]any
		wantErr bool
	}{
		{workflows.SignalPriceReview, map[string]any{"verdict": "APPROVED", "operator": "u"}, false},
		{workflows.SignalShipAction, map[string]any{"verdict": "SHIP", "qty": 5}, false},
		{workflows.SignalShipAction, map[string]any{"verdict": "SHIP"}, true},
		{workflows.SignalPaymentTracked, map[string]any{"amount": "9600"}, false},
		{workflows.SignalShadowInject, map[string]any{"verdict": "receipt-exception"}, false},
		{workflows.SignalReceiptProvided, map[string]any{}, false},
		{workflows.SignalModifyOrder, map[string]any{"class": "A", "changes": []any{"price"}}, false},
		{workflows.SignalModifyOrder, map[string]any{}, true},
	}
	for _, tc := range cases {
		_, err := TypedSignal(tc.signal, tc.payload)
		if tc.wantErr && err == nil {
			t.Fatalf("signal %s with payload %+v should fail", tc.signal, tc.payload)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("signal %s with payload %+v failed: %v", tc.signal, tc.payload, err)
		}
	}
}
