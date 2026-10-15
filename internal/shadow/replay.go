package shadow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"b2b_pilot_v3/internal/domain"
)

// Transition 一次待回放的状态迁移（影子事件链 order_events 的 from_state→to_state）。
type Transition struct {
	FSM        string
	From       string
	To         string
	Source     string // real | seed
	OccurredAt time.Time
}

// ReplayResult 回放结果（确定性：命中与分歧均按稳定顺序输出）。
type ReplayResult struct {
	TotalTransitions int
	CoveredEdges     map[string]int // key = EdgeKey
	Divergences      []Divergence
}

// EdgeKey 迁移边的稳定 key："FSM|from|to"。
func EdgeKey(fsm, from, to string) string { return fsm + "|" + from + "|" + to }

// SplitEdgeKey 反向解析 EdgeKey。
func SplitEdgeKey(key string) (fsm, from, to string, err error) {
	parts := strings.Split(key, "|")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("malformed edge key %q", key)
	}
	return parts[0], parts[1], parts[2], nil
}

// FSMByname 定位命名状态机（影子轨六台全集）。
func FSMByname(name string) (*domain.FSM, bool) {
	for _, info := range domain.AllFSMs() {
		if info.Name == name {
			return info.FSM, true
		}
	}
	return nil, false
}

// ReplayTransitions S-01：按事件链回放状态迁移。
// 合法边 → 覆盖计数 +1；非法边 → FSM_GAP 分歧（不计数）；未知 FSM → FSM_GAP 分歧。
// 同一输入两次 → 结果与覆盖计数一致（map 只增不减，无随机源）。
func ReplayTransitions(transitions []Transition) ReplayResult {
	res := ReplayResult{CoveredEdges: map[string]int{}}
	seen := map[string]bool{} // 分歧去重键（同 flow 同边只记一条）
	for _, tr := range transitions {
		res.TotalTransitions++
		fsm, ok := FSMByname(tr.FSM)
		if !ok {
			res.appendDivergence(seen, NewDivergence(KindFSMGap, SeverityP1, ReasonIllegalTransition, "",
				fmt.Sprintf("unknown fsm %q for %s -> %s", tr.FSM, tr.From, tr.To)))
			continue
		}
		if err := fsm.Validate(tr.From, tr.To); err != nil {
			res.appendDivergence(seen, NewDivergence(KindFSMGap, SeverityP1, ReasonIllegalTransition, "",
				err.Error()))
			continue
		}
		res.CoveredEdges[EdgeKey(tr.FSM, tr.From, tr.To)]++
	}
	return res
}

// appendDivergence 去重追加（同 kind|reason|detail 只保留首条；确定性）。
func (r *ReplayResult) appendDivergence(seen map[string]bool, d Divergence) {
	key := d.Kind + "|" + d.Reason + "|" + d.Detail
	if seen[key] {
		return
	}
	seen[key] = true
	r.Divergences = append(r.Divergences, d)
}

// ExhaustiveReplay S-01 穷举口径：回放 docs/v0.1/12 §3.1 的 79 条合法边。
// 任一边非法都是设计漂移（域状态机与影子文档不一致）。
func ExhaustiveReplay() (ReplayResult, []domain.Edge) {
	edges := domain.AllEdges()
	var trs []Transition
	for _, e := range edges {
		trs = append(trs, Transition{FSM: e.FSM, From: e.From, To: e.To, Source: "seed"})
	}
	return ReplayTransitions(trs), edges
}

// SortedCoverage 覆盖矩阵的确定性扁平输出（报告用）。
func (r ReplayResult) SortedCoverage() []EdgeHit {
	out := make([]EdgeHit, 0, len(r.CoveredEdges))
	for key, hits := range r.CoveredEdges {
		fsm, from, to, err := SplitEdgeKey(key)
		if err != nil {
			continue
		}
		out = append(out, EdgeHit{FSM: fsm, From: from, To: to, HitCount: hits})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FSM != out[j].FSM {
			return out[i].FSM < out[j].FSM
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// EdgeHit 一条边的命中计数。
type EdgeHit struct {
	FSM      string `json:"fsm"`
	From     string `json:"from"`
	To       string `json:"to"`
	HitCount int    `json:"hit_count"`
}
