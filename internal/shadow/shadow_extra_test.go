package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"b2b_pilot_v3/internal/workflows"
)

// ── 覆盖补齐（L0 纯函数全分支；让 shadow 包覆盖率不低于发布门禁）──

func TestSplitEdgeKey(t *testing.T) {
	fsm, from, to, err := SplitEdgeKey("ORD|A|B")
	if err != nil || fsm != "ORD" || from != "A" || to != "B" {
		t.Fatalf("split = %q,%q,%q err=%v", fsm, from, to, err)
	}
	if _, _, _, err := SplitEdgeKey("bad"); err == nil {
		t.Fatal("malformed edge key accepted")
	}
}

func TestFSMBynameUnknown(t *testing.T) {
	if _, ok := FSMByname("NOPE"); ok {
		t.Fatal("unknown fsm found")
	}
	for _, name := range []string{"ORD", "STK", "SHP", "INV", "PRE", "Gatekeeper"} {
		if _, ok := FSMByname(name); !ok {
			t.Fatalf("fsm %s missing", name)
		}
	}
}

func TestReplayUnknownFSMAndDivergenceDedup(t *testing.T) {
	res := ReplayTransitions([]Transition{
		{FSM: "NOPE", From: "A", To: "B"},
		{FSM: "NOPE", From: "A", To: "B"}, // 同 detail 去重
	})
	if len(res.Divergences) != 1 {
		t.Fatalf("divergences = %d, want dedup to 1", len(res.Divergences))
	}
	if res.TotalTransitions != 2 {
		t.Fatalf("total = %d, want 2", res.TotalTransitions)
	}
}

func TestSortedCoverageDeterministicAndMalformedSkipped(t *testing.T) {
	res := ReplayResult{CoveredEdges: map[string]int{
		"STK|B|A": 3,
		"ORD|A|B": 1,
		"bad":     9, // 畸形键应被跳过，不 panic
	}}
	hits := res.SortedCoverage()
	want := []EdgeHit{
		{FSM: "ORD", From: "A", To: "B", HitCount: 1},
		{FSM: "STK", From: "B", To: "A", HitCount: 3},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("sorted coverage = %+v, want %+v", hits, want)
	}
}

func TestClassifyDefaultAndValidateDivergence(t *testing.T) {
	kind, severity, action := ClassifyDivergence("totally-unknown")
	if kind != KindLegacyAnomaly || severity != SeverityP1 || action == "" {
		t.Fatalf("classify unknown = %s/%s/%q", kind, severity, action)
	}
	for _, d := range []Divergence{
		{Kind: "NOPE", Severity: SeverityP1, Reason: "x"},
		{Kind: KindMapping, Severity: "P9", Reason: "x"},
		{Kind: "", Severity: SeverityP1, Reason: "x"},
	} {
		if err := ValidateDivergence(d); err == nil {
			t.Fatalf("invalid divergence accepted: %+v", d)
		}
	}
}

func TestLoadFeedConfigErrorBranches(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadFeedConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing config accepted")
	}
	badJSON := filepath.Join(dir, "bad.json")
	os.WriteFile(badJSON, []byte("{"), 0o644)
	if _, err := LoadFeedConfig(badJSON); err == nil {
		t.Fatal("bad json accepted")
	}
	noTenant := filepath.Join(dir, "no-tenant.json")
	os.WriteFile(noTenant, []byte(`{"rules":[{"action":"a"}]}`), 0o644)
	if _, err := LoadFeedConfig(noTenant); err == nil {
		t.Fatal("missing tenant accepted")
	}
	noRules := filepath.Join(dir, "no-rules.json")
	os.WriteFile(noRules, []byte(`{"tenant_id":"t"}`), 0o644)
	if _, err := LoadFeedConfig(noRules); err == nil {
		t.Fatal("missing rules accepted")
	}
	dupRules := filepath.Join(dir, "dup.json")
	os.WriteFile(dupRules, []byte(`{"tenant_id":"t","rules":[{"stage_code":"ORD","action":"a"},{"stage_code":"ORD","action":"a"}]}`), 0o644)
	if _, err := LoadFeedConfig(dupRules); err == nil {
		t.Fatal("duplicate rules accepted")
	}
	noAction := filepath.Join(dir, "no-action.json")
	os.WriteFile(noAction, []byte(`{"tenant_id":"t","rules":[{"stage_code":"ORD"}]}`), 0o644)
	if _, err := LoadFeedConfig(noAction); err == nil {
		t.Fatal("missing action accepted")
	}
}

func TestTranslateWildcardAndVerdictField(t *testing.T) {
	cfg := &FeedConfig{TenantID: "t", Rules: []FeedRule{
		{StageCode: "*", Action: "任意段动作", Signal: "price-review", VerdictField: "decision",
			PayloadFields: []string{"operator"}},
		{StageCode: "ORD", Action: "start别名", Signal: "start", PayloadFields: []string{"customer_id"}},
	}}
	d, err := Translate(cfg, SourceEvent{StageCode: "INV", Action: "任意段动作",
		Payload: map[string]any{"operator": "u-op", "decision": "APPROVED"}})
	if err != nil || d.Signal != "price-review" || d.Payload["verdict"] != "APPROVED" {
		t.Fatalf("wildcard translate = %+v err=%v", d, err)
	}
	d, err = Translate(cfg, SourceEvent{StageCode: "ORD", Action: "start别名", Payload: map[string]any{"customer_id": "c"}})
	if err != nil || !d.Start {
		t.Fatalf("start translate = %+v err=%v", d, err)
	}
}

func TestClassifyTranslateErrorDefault(t *testing.T) {
	ev := SourceEvent{ID: 7, LegacyFlowID: "f", StageCode: "ORD", Action: "x"}
	d := classifyTranslateError(ev, errors.New("boom"))
	if d.Kind != KindMapping || d.SourceEventID != 7 {
		t.Fatalf("classify default = %+v", d)
	}
}

func TestPlanFeedPrestartedFlowAndDuplicateStart(t *testing.T) {
	cfg := sampleConfig()
	events := []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-pre", LegacyEventID: "e1", StageCode: "ORD", Action: "创建订单",
			Payload: map[string]any{"customer_id": "c"}},
		{ID: 2, LegacyFlowID: "flow-pre", LegacyEventID: "e2", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u"}},
	}
	plan := PlanFeedWithStarted(cfg, events, []string{"flow-pre"})
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != StepSignal {
		t.Fatalf("prestarted flow plan = %+v", plan)
	}
	if len(plan.SkippedEventIDs) != 1 || plan.SkippedEventIDs[0] != 1 {
		t.Fatalf("duplicate start event must be skipped: %+v", plan.SkippedEventIDs)
	}
	if len(plan.Divergences) != 0 {
		t.Fatalf("prestarted duplicate start must be silent idempotent: %+v", plan.Divergences)
	}
}

// ── TypedSignal / 字段读取全分支 ──

func TestTypedSignalAllBranches(t *testing.T) {
	cases := []struct {
		signal  string
		payload map[string]any
	}{
		{workflows.SignalPriceReview, map[string]any{"verdict": "APPROVED", "operator": "u", "reason": "ok"}},
		{workflows.SignalChairmanReview, map[string]any{"verdict": "APPROVED", "operator": "u"}},
		{workflows.SignalStockReview, map[string]any{"verdict": "APPROVED", "operator": "u", "warehouse_id": "wh", "note": "n"}},
		{workflows.SignalWarehouseConfirm, map[string]any{"verdict": "TRANSFER", "operator": "u"}},
		{workflows.SignalTransferDecision, map[string]any{"verdict": "APPROVE_TRANSFER", "operator": "u", "warehouse_id": "wh"}},
		{workflows.SignalPaymentConfirm, map[string]any{"verdict": "APPROVED", "operator": "u", "ledger_ref": "L1"}},
		{workflows.SignalShipAction, map[string]any{"verdict": "SHIP", "operator": "u", "qty": 2, "warehouse_id": "wh"}},
		{workflows.SignalReceiptExReview, map[string]any{"verdict": "APPROVED", "operator": "u", "deadline_days": "3"}},
		{workflows.SignalInvoiceIssued, map[string]any{"verdict": "ISSUED", "operator": "u", "tax_no": "T1"}},
		{workflows.SignalPaymentTracked, map[string]any{"verdict": "", "operator": "u", "amount": 100}},
		{workflows.SignalShadowInject, map[string]any{"verdict": "receipt-exception"}},
		{workflows.SignalResubmit, map[string]any{"operator": "u"}},
		{workflows.SignalModifyOrder, map[string]any{"class": "B", "changes": []string{"a"}, "operator": "u"}},
	}
	for _, tc := range cases {
		if _, err := TypedSignal(tc.signal, tc.payload); err != nil {
			t.Fatalf("TypedSignal(%s, %+v) = %v", tc.signal, tc.payload, err)
		}
	}
	if v, err := TypedSignal(workflows.SignalReceiptProvided, nil); err != nil || v != nil {
		t.Fatalf("receipt-provided = %v, %v", v, err)
	}
	if _, err := TypedSignal("not-a-signal", map[string]any{}); err == nil {
		t.Fatal("unknown signal accepted")
	}
	if _, err := TypedSignal(workflows.SignalPaymentTracked, map[string]any{}); err == nil {
		t.Fatal("payment-tracked without amount accepted")
	}
}

func TestSignalFieldHelpers(t *testing.T) {
	if got := StrOf(nil, "x", "d"); got != "d" {
		t.Fatalf("StrOf nil = %q", got)
	}
	if got := StrOf(map[string]any{"x": 3.2}, "x", "d"); got != "3.2" {
		t.Fatalf("StrOf numeric = %q", got)
	}
	for _, v := range []any{float64(3), json.Number("4"), "5", int(6), int8(7), int16(8), int32(9), int64(10), uint(11), uint8(12), uint16(13), uint32(14), uint64(15)} {
		if got := I64Of(map[string]any{"q": v}, "q", -1); got <= 0 {
			t.Fatalf("I64Of(%T) = %d", v, got)
		}
	}
	if got := I64Of(map[string]any{"q": "x"}, "q", 9); got != 9 {
		t.Fatalf("I64Of bad string = %d", got)
	}
	if got := I64Of(map[string]any{}, "q", 9); got != 9 {
		t.Fatalf("I64Of missing = %d", got)
	}
	if got := StrSlice([]string{"a", "b"}); len(got) != 2 || got[0] != "a" {
		t.Fatalf("StrSlice strings = %v", got)
	}
	if got := StrSlice("single"); len(got) != 1 || got[0] != "single" {
		t.Fatalf("StrSlice single = %v", got)
	}
	if got := StrSlice(""); got != nil {
		t.Fatalf("StrSlice empty = %v", got)
	}
	if got := StrSlice(42); got != nil {
		t.Fatalf("StrSlice other = %v", got)
	}
}
