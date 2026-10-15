package main

import (
	"strings"
	"testing"

	"b2b_pilot_v3/internal/shadow"
)

// S-08 · 四张数 markdown 渲染：覆盖率 / 分歧 / 对齐 / 卡住全部成段。
func TestRenderMarkdownFourSections(t *testing.T) {
	rep := &report{
		GeneratedAt: "2026-08-20T00:00:00Z",
		TenantID:    "chang-an",
		Coverage: coverageSection{
			TotalEdges: 79, HitEdges: 21, Percent: 26.6,
			ByFSM: map[string]fsmCov{
				"ORD": {Edges: 20, Hit: 5, Percent: 25, Real: 5},
			},
		},
		Divergences: shadowDivergenceSection{
			Total: 1, OpenP0: 0, OpenP1: 1,
			ByKind: []shadow.DivergenceSummary{{Kind: shadow.KindFSMGap, Severity: shadow.SeverityP1, Status: shadow.DivergenceOpen, Count: 1}},
		},
		Alignment: []shadow.StateAlignmentRow{{OrderID: "ORD-1", FlowID: "f1", ShadowState: "INV_CLOSED", SourceState: "INV_INVOICED", LastAction: "回款登记"}},
		Stuck:     []shadow.StuckOrder{{OrderID: "ORD-2", State: "STK_STOCK_REVIEWING", LastEventAge: "8 days", LastAction: "StockReviewStarted"}},
	}
	md := renderMarkdown(rep)
	for _, want := range []string{"FSM 边覆盖率", "21/79 (26.6%)", "分歧清单", "FSM_GAP", "状态对齐", "INV_CLOSED", "卡住清单", "ORD-2", "v0.2 角色迁入 Go 条件口径"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestHappyPathCoveredGate(t *testing.T) {
	by := map[string]fsmCov{
		"ORD": {Real: 1}, "STK": {Real: 1}, "SHP": {Real: 1}, "INV": {Real: 1},
	}
	if !happyPathCovered(by) {
		t.Fatal("four-segment real coverage must pass happy-path gate")
	}
	by["INV"] = fsmCov{Real: 0}
	if happyPathCovered(by) {
		t.Fatal("missing INV real coverage must fail happy-path gate")
	}
}
