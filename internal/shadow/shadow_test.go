package shadow

import (
	"reflect"
	"testing"
	"time"

	"b2b_pilot_v3/internal/domain"
)

// S-01 · FSM 回放器穷举：79 条合法边全通过；任意非法边 → 分歧 FSM_GAP；
// 重放同输入两次 → 结果与覆盖计数一致（12 §10）。
func TestS01ExhaustiveReplay(t *testing.T) {
	res, edges := ExhaustiveReplay()
	if got := len(edges); got != 79 {
		t.Fatalf("edge count drift: got %d, design 79", got)
	}
	if len(res.Divergences) != 0 {
		t.Fatalf("legal edges produced divergences: %+v", res.Divergences)
	}
	if len(res.CoveredEdges) != 79 {
		t.Fatalf("covered edges = %d, want 79", len(res.CoveredEdges))
	}
	for _, e := range edges {
		if res.CoveredEdges[EdgeKey(e.FSM, e.From, e.To)] != 1 {
			t.Fatalf("edge %s|%s|%s not covered", e.FSM, e.From, e.To)
		}
	}
}

func TestS01StateAndEdgeInventory(t *testing.T) {
	states := 0
	for _, info := range domain.AllFSMs() {
		states += len(info.FSM.States())
	}
	if states != 52 {
		t.Fatalf("state count drift: got %d, design 52", states)
	}
}

func TestS01IllegalEdgeProducesFSMGap(t *testing.T) {
	res := ReplayTransitions([]Transition{
		{FSM: "ORD", From: domain.StateORDRequested, To: domain.StateORDRejected}, // 合法
		{FSM: "ORD", From: domain.StateORDRequested, To: domain.StateINVClosed},   // 非法
	})
	if len(res.Divergences) != 1 {
		t.Fatalf("divergences = %d, want 1", len(res.Divergences))
	}
	d := res.Divergences[0]
	if d.Kind != KindFSMGap || d.Reason != ReasonIllegalTransition {
		t.Fatalf("divergence = %+v", d)
	}
	if res.CoveredEdges[EdgeKey("ORD", domain.StateORDRequested, domain.StateORDRejected)] != 1 {
		t.Fatal("legal edge not counted")
	}
}

func TestS01ReplayDeterministic(t *testing.T) {
	trs := []Transition{
		{FSM: "ORD", From: domain.StateORDRequested, To: domain.StateORDPriceReviewing},
		{FSM: "STK", From: domain.StateSTKReadyForStock, To: domain.StateSTKStockReviewing},
		{FSM: "ORD", From: domain.StateORDRequested, To: domain.StateINVClosed},
	}
	a, b := ReplayTransitions(trs), ReplayTransitions(trs)
	if !reflect.DeepEqual(a.CoveredEdges, b.CoveredEdges) {
		t.Fatalf("coverage differs between identical replays")
	}
	if len(a.Divergences) != len(b.Divergences) {
		t.Fatalf("divergence count differs between identical replays")
	}
	for i := range a.Divergences {
		if a.Divergences[i] != b.Divergences[i] {
			t.Fatalf("divergence %d differs between identical replays", i)
		}
	}
}

// S-02 · 映射配置：已知动作正确翻译；未知动作 → UNMAPPED_EVENT；
// 缺必填字段不投递并留痕（12 §10）。
func TestS02TranslateKnownAndUnknown(t *testing.T) {
	cfg := &FeedConfig{TenantID: "chang-an", Rules: []FeedRule{
		{StageCode: "ORD", Action: "创建订单", Signal: "start", PayloadFields: []string{"customer_id", "payment_term"}},
		{StageCode: "ORD", Action: "主管通过", Signal: "price-review", Verdict: "APPROVED", PayloadFields: []string{"operator", "reason"}},
		{StageCode: "SHP", Action: "仓管确认发货", Signal: "ship-action", VerdictField: "verdict", Required: []string{"qty"}},
	}}
	ev := SourceEvent{StageCode: "ORD", Action: "主管通过", Payload: map[string]any{"operator": "u-supervisor", "reason": "ok"}}
	d, err := Translate(cfg, ev)
	if err != nil {
		t.Fatalf("known action rejected: %v", err)
	}
	if d.Start || d.Signal != "price-review" || d.Payload["verdict"] != "APPROVED" {
		t.Fatalf("delivery = %+v", d)
	}

	if _, err := Translate(cfg, SourceEvent{StageCode: "ORD", Action: "未知动作", Payload: map[string]any{}}); err == nil {
		t.Fatal("unknown action translated (must never guess)")
	} else if _, ok := err.(*ErrUnmapped); !ok {
		t.Fatalf("unknown action error = %T", err)
	}

	_, err = Translate(cfg, SourceEvent{StageCode: "SHP", Action: "仓管确认发货", Payload: map[string]any{"verdict": "SHIP"}})
	if err == nil {
		t.Fatal("missing required field translated")
	} else if _, ok := err.(*ErrMissingField); !ok {
		t.Fatalf("missing field error = %T", err)
	}
}

// S-03 · 分歧四分类判定纯函数穷举；幂等键（flow_id + source_event_id）重复投递只记录一次。
func TestS03DivergenceClassificationMatrix(t *testing.T) {
	cases := []struct {
		reason   string
		kind     string
		severity string
	}{
		{ReasonUnmappedEvent, KindMapping, SeverityP2},
		{ReasonMissingField, KindMapping, SeverityP2},
		{ReasonIllegalTransition, KindFSMGap, SeverityP1},
		{ReasonTimeInversion, KindDataGap, SeverityP2},
		{ReasonNoCreationEvent, KindDataGap, SeverityP2},
		{ReasonChainBreak, KindLegacyAnomaly, SeverityP1},
		{ReasonStateMismatch, KindLegacyAnomaly, SeverityP1},
		{ReasonStuckWaitingSignal, KindLegacyAnomaly, SeverityP1},
	}
	for _, tc := range cases {
		kind, severity, action := ClassifyDivergence(tc.reason)
		if kind != tc.kind || severity != tc.severity || action == "" {
			t.Fatalf("classify(%s) = (%s,%s,%q)", tc.reason, kind, severity, action)
		}
		d := NewDivergence(kind, "", tc.reason, "flow-1", "detail")
		if err := ValidateDivergence(d); err != nil {
			t.Fatalf("classifier produced invalid divergence: %v", err)
		}
	}
}

func sampleEvents() []SourceEvent {
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	return []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-1", LegacyEventID: "e1", StageCode: "ORD", Action: "创建订单",
			Payload: map[string]any{"customer_id": "cust-1", "payment_term": "款到"}, OccurredAt: base},
		{ID: 2, LegacyFlowID: "flow-1", LegacyEventID: "e2", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u-supervisor"}, OccurredAt: base.Add(time.Hour)},
		{ID: 3, LegacyFlowID: "flow-1", LegacyEventID: "e3", StageCode: "STK", Action: "商务备货通过",
			Payload: map[string]any{"operator": "u-op", "warehouse_id": "wh-yangyuan"}, OccurredAt: base.Add(2 * time.Hour)},
	}
}

func sampleConfig() *FeedConfig {
	return &FeedConfig{
		TenantID: "chang-an",
		Rules: []FeedRule{
			{StageCode: "ORD", Action: "创建订单", Signal: "start", PayloadFields: []string{"customer_id", "payment_term"}},
			{StageCode: "ORD", Action: "主管通过", Signal: "price-review", Verdict: "APPROVED", PayloadFields: []string{"operator"}},
			{StageCode: "STK", Action: "商务备货通过", Signal: "stock-review", Verdict: "APPROVED",
				PayloadFields: []string{"operator", "warehouse_id"}},
		},
	}
}

// S-05 · shadow-feed E2E 计划：建单 → price-review → stock-review 全链投递推进，
// 每步事件链与 FSM 一致（执行侧由 cmd/shadow-feed + Temporal 完成；本测试验证计划确定性）。
func TestS05PlanFullChainDeterministicAndIdempotent(t *testing.T) {
	cfg := sampleConfig()
	events := sampleEvents()
	a, b := PlanFeed(cfg, events), PlanFeed(cfg, events)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("plan not deterministic:\na=%+v\nb=%+v", a, b)
	}
	if len(a.Steps) != 3 || a.Steps[0].Kind != StepStartOrder {
		t.Fatalf("steps = %+v", a.Steps)
	}
	if a.Steps[1].Delivery.Signal != "price-review" || a.Steps[2].Delivery.Signal != "stock-review" {
		t.Fatalf("signal order = %+v", a.Steps)
	}
	if len(a.Divergences) != 0 {
		t.Fatalf("happy chain produced divergences: %+v", a.Divergences)
	}

	// 幂等：(flow_id, source_event_id) 重复投递只记录一次
	dup := append(append([]SourceEvent(nil), events...), events[1])
	plan := PlanFeed(cfg, dup)
	if len(plan.Steps) != 3 || len(plan.Divergences) != 0 {
		t.Fatalf("duplicate event changed plan: steps=%d div=%d", len(plan.Steps), len(plan.Divergences))
	}
}

func TestS05PlanOrphanEventsAndUnknownActions(t *testing.T) {
	cfg := sampleConfig()
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	events := []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-x", LegacyEventID: "x1", StageCode: "STK", Action: "商务备货通过",
			Payload: map[string]any{"operator": "u-op", "warehouse_id": "wh-yangyuan"}, OccurredAt: base},
		{ID: 2, LegacyFlowID: "flow-x", LegacyEventID: "x2", StageCode: "ORD", Action: "未知动作", OccurredAt: base.Add(time.Hour)},
	}
	plan := PlanFeed(cfg, events)
	if len(plan.Steps) != 0 {
		t.Fatalf("orphan flow steps = %+v (chain break must not deliver)", plan.Steps)
	}
	if len(plan.Divergences) != 2 {
		t.Fatalf("divergences = %+v, want 2 (unknown + chain break)", plan.Divergences)
	}
	kinds := map[string]bool{}
	for _, d := range plan.Divergences {
		kinds[d.Reason] = true
	}
	if !kinds[ReasonUnmappedEvent] || !kinds[ReasonNoCreationEvent] {
		t.Fatalf("missing expected divergence reasons: %+v", kinds)
	}
}

func TestS05PlanTimeInversionRecorded(t *testing.T) {
	cfg := sampleConfig()
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	events := []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-1", LegacyEventID: "e1", StageCode: "ORD", Action: "创建订单",
			Payload: map[string]any{"customer_id": "cust-1"}, OccurredAt: base.Add(time.Hour)},
		{ID: 2, LegacyFlowID: "flow-1", LegacyEventID: "e2", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u-supervisor"}, OccurredAt: base},
	}
	plan := PlanFeed(cfg, events)
	found := false
	for _, d := range plan.Divergences {
		if d.Reason == ReasonTimeInversion {
			found = true
		}
	}
	if !found {
		t.Fatalf("time inversion not recorded: %+v", plan.Divergences)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (inversion recorded but flow still processed)", len(plan.Steps))
	}
}
