// cmd/shadow-audit —— 影子轨每日审计（docs/v0.1/12-shadow-full-flow.html §5 步骤 7-8 / §8.2）
//
// 四张数：FSM 边覆盖率（79 边 · 按 ORD/STK/SHP/INV/PRE/Gatekeeper 分组）·
// 分歧清单（P0/P1/P2 × 四分类）· 状态对齐（影子终态 vs 源快照）· 卡住清单（等待信号 > N 天）。
// 输出 markdown + JSON 到 SHADOW_REPORT_DIR（默认 logs/audit/shadow/），与影子库同源可复核（S-08）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/shadow"
	"b2b_pilot_v3/internal/shadowguard"
)

type report struct {
	GeneratedAt string                     `json:"generated_at"`
	TenantID    string                     `json:"tenant_id"`
	Coverage    coverageSection            `json:"coverage"`
	Divergences shadowDivergenceSection    `json:"divergences"`
	Alignment   []shadow.StateAlignmentRow `json:"state_alignment"`
	Stuck       []shadow.StuckOrder        `json:"stuck_orders"`
	GoV02       goV02                      `json:"v02_go_conditions"`
}

type coverageSection struct {
	TotalEdges int               `json:"total_edges"`
	HitEdges   int               `json:"hit_edges"`
	Percent    float64           `json:"percent"`
	ByFSM      map[string]fsmCov `json:"by_fsm"`
}

type fsmCov struct {
	Edges   int     `json:"edges"`
	Hit     int     `json:"hit"`
	Percent float64 `json:"percent"`
	Real    int     `json:"real_hits"`
	Seed    int     `json:"seed_hits"`
}

type shadowDivergenceSection struct {
	Total  int                        `json:"total"`
	OpenP0 int                        `json:"open_p0"`
	OpenP1 int                        `json:"open_p1"`
	ByKind []shadow.DivergenceSummary `json:"by_kind"`
}

type goV02 struct {
	OrdStkShpInvHappyPathCovered bool    `json:"ord_stk_shp_inv_happy_path_covered"`
	TotalEdgeCoveragePct         float64 `json:"total_edge_coverage_pct"`
	NoOpenP0P1                   bool    `json:"no_open_p0_p1"`
}

func main() {
	dsn := flag.String("dsn", "", "影子库 DSN（默认取 B2B_PG_DSN）")
	tenant := flag.String("tenant", "", "租户（默认取 config/shadow-feed 或 chang-an）")
	stuckAfter := flag.Duration("stuck-after", 7*24*time.Hour, "卡住判定阈值（等待信号超过该时长）")
	interval := flag.Duration("interval", 0, "审计周期（如 24h）；0 = 只跑一轮后退出")
	stateFile := flag.String("state-file", "", "每轮成功后的探针文件（interval>0 时必填，供健康检查）")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if *interval < 0 {
		log.Fatalf("interval must be >= 0")
	}
	if *interval > 0 && *stateFile == "" {
		log.Fatalf("-state-file is required when -interval is set (compose healthcheck)")
	}

	cfg := config.Load()
	if *dsn == "" {
		*dsn = cfg.PGDSN
	}
	cfg.PGDSN = *dsn
	if err := shadowguard.Validate(cfg); err != nil {
		log.Fatalf("shadow guard: %v", err)
	}
	ctx := context.Background()
	store, err := shadow.Open(ctx, *dsn)
	if err != nil {
		log.Fatalf("open shadow pg: %v", err)
	}
	defer store.Close()

	if *tenant == "" {
		*tenant = "chang-an"
		if feedCfg, err := shadow.LoadFeedConfig(cfg.ShadowFeedConfig); err == nil {
			*tenant = feedCfg.TenantID
		}
	}

	run := func() {
		rep, err := buildReport(ctx, store, *tenant, *stuckAfter)
		if err != nil {
			log.Printf("audit run failed: %v", err)
			return
		}
		md := renderMarkdown(rep)
		fmt.Print(md)

		dir := cfg.ShadowReportDir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("mkdir report dir: %v", err)
			return
		}
		day := time.Now().UTC().Format("20060102")
		mdPath := filepath.Join(dir, fmt.Sprintf("shadow-audit-%s.md", day))
		jsonPath := filepath.Join(dir, fmt.Sprintf("shadow-audit-%s.json", day))
		if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
			log.Printf("write markdown: %v", err)
			return
		}
		jb, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			log.Printf("marshal json: %v", err)
			return
		}
		if err := os.WriteFile(jsonPath, jb, 0o644); err != nil {
			log.Printf("write json: %v", err)
			return
		}
		touchStateFile(*stateFile)
		log.Printf("report written: %s (%d bytes) + %s", mdPath, len(md), jsonPath)
	}

	run()
	if *interval == 0 {
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		run()
	}
}

// touchStateFile 写健康探针文件（内容为完成时间戳）。
func touchStateFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		log.Printf("write state file: %v", err)
	}
}

func buildReport(ctx context.Context, store *shadow.Store, tenantID string, stuckAfter time.Duration) (*report, error) {
	// 1. 覆盖率：79 边基线（seed 占位）+ 真实事件链扫描
	if _, err := store.EnsureCoverageBaseline(ctx, "seed"); err != nil {
		return nil, fmt.Errorf("coverage baseline: %w", err)
	}
	transitions, err := store.ListOrderTransitions(ctx, tenantID, 0)
	if err != nil {
		return nil, fmt.Errorf("scan transitions: %w", err)
	}
	type edgeAgg struct {
		fsm, from, to string
		hits          int64
		first, last   time.Time
	}
	agg := map[string]*edgeAgg{}
	for _, tr := range transitions {
		fsm := shadow.FSMForStates(tr.FromState, tr.ToState)
		if fsm == "" {
			continue
		}
		key := shadow.EdgeKey(fsm, tr.FromState, tr.ToState)
		e, ok := agg[key]
		if !ok {
			e = &edgeAgg{fsm: fsm, from: tr.FromState, to: tr.ToState, first: tr.OccurredAt, last: tr.OccurredAt}
			agg[key] = e
		}
		e.hits++
		if tr.OccurredAt.Before(e.first) {
			e.first = tr.OccurredAt
		}
		if tr.OccurredAt.After(e.last) {
			e.last = tr.OccurredAt
		}
	}
	// 用 SetCoverage 重算（幂等口径：同一审计日重跑不重复累加）。
	for _, e := range agg {
		if err := store.SetCoverage(ctx, e.fsm, e.from, e.to, e.hits, e.first, e.last, "real"); err != nil {
			return nil, fmt.Errorf("set coverage %s %s->%s: %w", e.fsm, e.from, e.to, err)
		}
	}
	covRows, err := store.ListCoverage(ctx)
	if err != nil {
		return nil, fmt.Errorf("list coverage: %w", err)
	}
	cov := coverageSection{ByFSM: map[string]fsmCov{}}
	for _, r := range covRows {
		c := cov.ByFSM[r.FSM]
		c.Edges++
		if r.HitCount > 0 {
			c.Hit++
			if r.Source == "real" {
				c.Real++
			} else {
				c.Seed++
			}
		}
		cov.ByFSM[r.FSM] = c
	}
	for k, c := range cov.ByFSM {
		if c.Edges > 0 {
			c.Percent = 100 * float64(c.Hit) / float64(c.Edges)
			cov.ByFSM[k] = c
		}
	}
	cov.TotalEdges = len(covRows)
	cov.HitEdges = 0
	for _, c := range cov.ByFSM {
		cov.HitEdges += c.Hit
	}
	if cov.TotalEdges > 0 {
		cov.Percent = 100 * float64(cov.HitEdges) / float64(cov.TotalEdges)
	}

	// 2. 分歧清单
	divSum, err := store.ListDivergenceSummary(ctx)
	if err != nil {
		return nil, fmt.Errorf("divergence summary: %w", err)
	}
	div := shadowDivergenceSection{ByKind: divSum}
	for _, d := range divSum {
		div.Total += d.Count
		if d.Status == shadow.DivergenceOpen {
			if d.Severity == shadow.SeverityP0 {
				div.OpenP0 += d.Count
			}
			if d.Severity == shadow.SeverityP1 {
				div.OpenP1 += d.Count
			}
		}
	}

	// 3. 状态对齐 / 4. 卡住清单
	align, err := store.ListStateAlignment(ctx, tenantID, 500)
	if err != nil {
		return nil, fmt.Errorf("state alignment: %w", err)
	}
	stuck, err := store.ListStuckOrders(ctx, tenantID, stuckAfter)
	if err != nil {
		return nil, fmt.Errorf("stuck orders: %w", err)
	}

	// v0.2 角色迁入 Go 条件口径（12 §8.3；建议写入门禁，不自动裁决）
	g := goV02{
		TotalEdgeCoveragePct: cov.Percent,
		NoOpenP0P1:           div.OpenP0 == 0 && div.OpenP1 == 0,
	}
	g.OrdStkShpInvHappyPathCovered = happyPathCovered(cov.ByFSM)
	return &report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		TenantID:    tenantID,
		Coverage:    cov,
		Divergences: div,
		Alignment:   align,
		Stuck:       stuck,
		GoV02:       g,
	}, nil
}

// happyPathCovered ORD/STK/SHP/INV 四主段各至少命中 1 边（真实数据）；其余按 §8.3 阈值人工裁决。
func happyPathCovered(byFSM map[string]fsmCov) bool {
	for _, fsm := range []string{"ORD", "STK", "SHP", "INV"} {
		if byFSM[fsm].Real == 0 {
			return false
		}
	}
	return true
}

func renderMarkdown(rep *report) string {
	var b strings.Builder
	b.WriteString("# Shadow Full-Flow Audit\n\n")
	b.WriteString(fmt.Sprintf("- generated_at: %s\n", rep.GeneratedAt))
	b.WriteString(fmt.Sprintf("- tenant_id: %s\n", rep.TenantID))
	b.WriteString(fmt.Sprintf("- FSM edge coverage: **%d/%d (%.1f%%)**\n", rep.Coverage.HitEdges, rep.Coverage.TotalEdges, rep.Coverage.Percent))
	b.WriteString(fmt.Sprintf("- divergences: total=%d open_p0=%d open_p1=%d\n", rep.Divergences.Total, rep.Divergences.OpenP0, rep.Divergences.OpenP1))
	b.WriteString(fmt.Sprintf("- state alignment mismatches: %d\n", len(rep.Alignment)))
	b.WriteString(fmt.Sprintf("- stuck orders: %d\n\n", len(rep.Stuck)))

	b.WriteString("## FSM 边覆盖率\n\n| FSM | 边 | 命中 | % | real | seed |\n|---|---|---|---|---|---|\n")
	keys := make([]string, 0, len(rep.Coverage.ByFSM))
	for k := range rep.Coverage.ByFSM {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := rep.Coverage.ByFSM[k]
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %.1f | %d | %d |\n", k, c.Edges, c.Hit, c.Percent, c.Real, c.Seed))
	}

	b.WriteString("\n## 分歧清单（kind × severity × status）\n\n| kind | severity | status | count |\n|---|---|---|---|\n")
	for _, d := range rep.Divergences.ByKind {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n", d.Kind, d.Severity, d.Status, d.Count))
	}
	if len(rep.Divergences.ByKind) == 0 {
		b.WriteString("| - | - | - | 0 |\n")
	}

	b.WriteString("\n## 状态对齐（影子终态 vs 源快照，容忍一个同步周期滞后）\n\n")
	if len(rep.Alignment) == 0 {
		b.WriteString("无显式快照状态不等。\n")
	} else {
		b.WriteString("| order | flow | shadow | source | last_action |\n|---|---|---|---|---|\n")
		for _, r := range rep.Alignment {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n", r.OrderID, r.FlowID, r.ShadowState, r.SourceState, r.LastAction))
		}
	}

	b.WriteString("\n## 卡住清单\n\n")
	if len(rep.Stuck) == 0 {
		b.WriteString("无卡住影子工作流。\n")
	} else {
		b.WriteString("| order | state | last_age | last_action |\n|---|---|---|---|\n")
		for _, s := range rep.Stuck {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", s.OrderID, s.State, s.LastEventAge, s.LastAction))
		}
	}

	b.WriteString("\n## v0.2 角色迁入 Go 条件口径（建议，不自动裁决）\n\n")
	b.WriteString(fmt.Sprintf("- ORD/STK/SHP/INV 四主段真实数据均有命中: %v\n", rep.GoV02.OrdStkShpInvHappyPathCovered))
	b.WriteString(fmt.Sprintf("- 总边覆盖 ≥ 60%%: %v (%.1f%%)\n", rep.Coverage.Percent >= 60, rep.Coverage.Percent))
	b.WriteString(fmt.Sprintf("- 无未裁决 P0/P1: %v\n", rep.GoV02.NoOpenP0P1))
	return b.String()
}
