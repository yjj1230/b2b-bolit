package shadow

import (
	"reflect"
	"testing"
	"time"
)

// ── 发布后场景倒推测试（CQ-11/12/14 + R1/R2 增量运行 + 映射修复闭环）──
// 这些用例把「上线之后每天会发生什么」翻译成可重放的纯函数断言：
//   - CQ-14 重复/并发类操作必须幂等；
//   - CQ-12 事件链计划必须确定性可回放；
//   - 映射修复后必须能通过 SKIPPED→NEW 的语义重新纳入计划。

func releaseEvents(base time.Time) []SourceEvent {
	return []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-a", LegacyEventID: "a1", StageCode: "ORD", Action: "创建订单",
			Payload: map[string]any{"customer_id": "c1"}, OccurredAt: base},
		{ID: 2, LegacyFlowID: "flow-a", LegacyEventID: "a2", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u"}, OccurredAt: base.Add(time.Hour)},
		{ID: 3, LegacyFlowID: "flow-b", LegacyEventID: "b1", StageCode: "ORD", Action: "创建订单",
			Payload: map[string]any{"customer_id": "c2"}, OccurredAt: base.Add(2 * time.Hour)},
	}
}

// 发布后 R1/R2 增量运行：第一轮建单并投递早期信号；第二轮只有新信号，已有流
// 不误判为链断裂、重复事件不再投递（CQ-14 / D-009 差异可见）。
func TestReleaseScenarioIncrementalRounds(t *testing.T) {
	cfg := sampleConfig()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

	round1 := PlanFeed(cfg, releaseEvents(base))
	if len(round1.Steps) != 3 || len(round1.Divergences) != 0 {
		t.Fatalf("round1 = steps:%d div:%d", len(round1.Steps), len(round1.Divergences))
	}
	started := []string{"flow-a", "flow-b"}

	// 第二轮：新事件 + 一条重复事件。已开始流直接接续，重复事件静默幂等。
	round2Events := []SourceEvent{
		{ID: 4, LegacyFlowID: "flow-a", LegacyEventID: "a3", StageCode: "STK", Action: "商务备货通过",
			Payload:    map[string]any{"operator": "u-op", "warehouse_id": "wh-yangyuan"},
			OccurredAt: base.Add(3 * time.Hour)},
		{ID: 4, LegacyFlowID: "flow-a", LegacyEventID: "a3", StageCode: "STK", Action: "商务备货通过",
			Payload:    map[string]any{"operator": "u-op", "warehouse_id": "wh-yangyuan"},
			OccurredAt: base.Add(3 * time.Hour)},
	}
	round2 := PlanFeedWithStarted(cfg, round2Events, started)
	if len(round2.Steps) != 1 || round2.Steps[0].Delivery.Signal != "stock-review" {
		t.Fatalf("round2 steps = %+v", round2.Steps)
	}
	if len(round2.Divergences) != 0 {
		t.Fatalf("round2 must not flag chain break for started flows: %+v", round2.Divergences)
	}
	if len(round2.SkippedEventIDs) != 0 {
		t.Fatalf("duplicate event must be silently idempotent, not skipped-recorded: %+v", round2.SkippedEventIDs)
	}
}

// 映射修复闭环：未映射事件先进入 SKIPPED + 分歧；补上映射规则后，同一事件
// 重新翻译为可投递步骤（操作上对应 make shadow-reset-skipped → 重放）。
func TestReleaseScenarioMappingFixReplay(t *testing.T) {
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	ev := SourceEvent{ID: 9, LegacyFlowID: "flow-fix", LegacyEventID: "f1",
		StageCode: "INV", Action: "回款登记", Payload: map[string]any{"amount": 100, "operator": "u"},
		OccurredAt: base}

	broken := &FeedConfig{TenantID: "t", Rules: []FeedRule{
		{StageCode: "ORD", Action: "创建订单", Signal: "start", PayloadFields: []string{"customer_id"}},
	}}
	before := PlanFeedWithStarted(broken, []SourceEvent{ev}, []string{"flow-fix"})
	if len(before.Divergences) != 1 || before.Divergences[0].Reason != ReasonUnmappedEvent {
		t.Fatalf("broken mapping divergence = %+v", before.Divergences)
	}
	if len(before.SkippedEventIDs) != 1 || before.SkippedEventIDs[0] != ev.ID {
		t.Fatalf("skipped ids = %+v", before.SkippedEventIDs)
	}

	fixed := &FeedConfig{TenantID: "t", Rules: append(broken.Rules, FeedRule{
		StageCode: "INV", Action: "回款登记", Signal: "payment-tracked",
		PayloadFields: []string{"amount", "operator"},
	})}
	after := PlanFeedWithStarted(fixed, []SourceEvent{ev}, []string{"flow-fix"})
	if len(after.Divergences) != 0 || len(after.Steps) != 1 {
		t.Fatalf("after fix plan = steps:%d div:%+v", len(after.Steps), after.Divergences)
	}
	if after.Steps[0].Delivery.Signal != "payment-tracked" {
		t.Fatalf("after fix delivery = %+v", after.Steps[0].Delivery)
	}
}

// 旧系统真实语义：已知但不驱动新系统状态机的状态（如 CHAIRMAN_SKIPPED /
// CONTRACT_STUB_CREATED）显式映射为 ignore——不投递、不写分歧、置 SKIPPED。
func TestReleaseScenarioKnownStatusesIgnored(t *testing.T) {
	cfg := &FeedConfig{TenantID: "t", Rules: []FeedRule{
		{StageCode: "ORD", Action: "CHAIRMAN_SKIPPED", Signal: "ignore"},
		{StageCode: "ORD", Action: "CONTRACT_STUB_CREATED", Signal: "ignore"},
		{StageCode: "ORD", Action: "SUPERVISOR_APPROVED", Signal: "price-review", Verdict: "APPROVED", PayloadFields: []string{"operator"}},
	}}
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	events := []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-real", LegacyEventID: "r1", StageCode: "ORD", Action: "CHAIRMAN_SKIPPED", OccurredAt: base},
		{ID: 2, LegacyFlowID: "flow-real", LegacyEventID: "r2", StageCode: "ORD", Action: "CONTRACT_STUB_CREATED", OccurredAt: base.Add(time.Hour)},
		{ID: 3, LegacyFlowID: "flow-real", LegacyEventID: "r3", StageCode: "ORD", Action: "SUPERVISOR_APPROVED",
			Payload: map[string]any{"operator": "u-supervisor"}, OccurredAt: base.Add(2 * time.Hour)},
	}
	plan := PlanFeedWithStarted(cfg, events, []string{"flow-real"})
	if len(plan.Steps) != 1 || plan.Steps[0].Delivery.Signal != "price-review" {
		t.Fatalf("steps = %+v (ignore statuses must not deliver)", plan.Steps)
	}
	if len(plan.Divergences) != 0 {
		t.Fatalf("known-ignore statuses must not create divergence: %+v", plan.Divergences)
	}
	if len(plan.SkippedEventIDs) != 2 {
		t.Fatalf("skipped = %+v, want the two ignore events", plan.SkippedEventIDs)
	}
}

// 旧系统最新代码库（tripartite-approval @ f3ed1f80）状态语义基线：
// 权威配置里的真实 status 枚举必须翻译正确，已知-不驱动状态显式 ignore，
// 未知状态仍然 UNMAPPED（不猜测）。
func TestReleaseScenarioRealFeishuStatusBaseline(t *testing.T) {
	cfg, err := LoadFeedConfig("../../config/shadow-feed.example.json")
	if err != nil {
		t.Fatalf("load real-status config: %v", err)
	}
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		action  string
		payload map[string]any
		want    string
	}{
		{"ORDER_SUBMIT_ACCEPTED", map[string]any{"order_id": "SO-1"}, "__start__"},
		{"MANAGER_APPROVED", map[string]any{}, "ignore"},
		{"SUPERVISOR_APPROVED", map[string]any{"operator": "u"}, "price-review"},
		{"CHAIRMAN_SKIPPED", map[string]any{}, "ignore"},
		{"CONTRACT_STUB_CREATED", map[string]any{}, "ignore"},
		{"STOCK_RESERVATION_APPROVED", map[string]any{"operator": "u", "warehouse_id": "wh"}, "stock-review"},
		{"WAREHOUSE_SHIPPED", map[string]any{"operator": "u", "qty": 5}, "ship-action"},
		{"COMPLETION_EVIDENCE_SUBMITTED", map[string]any{}, "receipt-provided"},
		{"FINANCE_ISSUE_APPROVED", map[string]any{"operator": "u"}, "invoice-issued"},
		{"NO_INVOICE_SALE_HANDLED", map[string]any{"operator": "u"}, "invoice-issued"},
	}
	for _, tc := range cases {
		ev := SourceEvent{ID: 1, LegacyFlowID: "flow-baseline", LegacyEventID: "e-" + tc.action,
			StageCode: "ORD", Action: tc.action, Payload: tc.payload, OccurredAt: base}
		d, err := Translate(cfg, ev)
		if err != nil {
			t.Fatalf("translate %s: %v", tc.action, err)
		}
		if tc.want == "__start__" {
			if !d.Start {
				t.Fatalf("%s should be start, got %+v", tc.action, d)
			}
			continue
		}
		if d.Signal != tc.want {
			t.Fatalf("%s signal = %q, want %q", tc.action, d.Signal, tc.want)
		}
	}
	if _, err := Translate(cfg, SourceEvent{StageCode: "ORD", Action: "未知状态", Payload: map[string]any{}}); err == nil {
		t.Fatal("unknown real status must remain UNMAPPED")
	}
}

// 确定性：同输入两次计划（含多流链断裂）完全一致（CQ-12 可回放）。
func TestReleaseScenarioDeterministicWithMultipleBrokenFlows(t *testing.T) {
	cfg := sampleConfig()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	events := []SourceEvent{
		{ID: 1, LegacyFlowID: "flow-x", LegacyEventID: "x1", StageCode: "STK", Action: "商务备货通过",
			Payload: map[string]any{"operator": "u", "warehouse_id": "wh"}, OccurredAt: base},
		{ID: 2, LegacyFlowID: "flow-z", LegacyEventID: "z1", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u"}, OccurredAt: base.Add(time.Hour)},
		{ID: 3, LegacyFlowID: "flow-a", LegacyEventID: "a1", StageCode: "ORD", Action: "主管通过",
			Payload: map[string]any{"operator": "u"}, OccurredAt: base.Add(2 * time.Hour)},
	}
	a, b := PlanFeed(cfg, events), PlanFeed(cfg, events)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("plan not deterministic:\na=%+v\nb=%+v", a, b)
	}
	if len(a.Divergences) != 3 {
		t.Fatalf("all three flows must be chain-break divergences: %+v", a.Divergences)
	}
	// 多流断裂的分歧顺序必须稳定（flow 排序），否则重放报告每天不一样。
	for i := 1; i < len(a.Divergences); i++ {
		if a.Divergences[i].LegacyFlowID < a.Divergences[i-1].LegacyFlowID {
			t.Fatalf("divergence order not sorted by flow: %+v", a.Divergences)
		}
	}
}
