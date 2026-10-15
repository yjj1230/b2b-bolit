package domain

import (
	"fmt"
	"sort"
)

// FSMInfo 命名状态机（影子覆盖率/回放器枚举用，docs/v0.1/12 §3.1）。
type FSMInfo struct {
	Name string
	FSM  *FSM
}

// AllFSMs 返回 v0.1 全量六台状态机（ORD/STK/SHP/INV/PRE/Gatekeeper）。
// 顺序固定：主流程四段 → 预订单 → 守门员。
func AllFSMs() []FSMInfo {
	return []FSMInfo{
		{Name: "ORD", FSM: OrdFSM},
		{Name: "STK", FSM: StkFSM},
		{Name: "SHP", FSM: ShpFSM},
		{Name: "INV", FSM: InvFSM},
		{Name: "PRE", FSM: PreOrderFSM},
		{Name: "Gatekeeper", FSM: GatekeeperFSM},
	}
}

// Edge 一条去重后的 FSM 迁移边。
type Edge struct {
	FSM  string
	From string
	To   string
}

// AllEdges 六台状态机全部合法迁移边（去重；按 FSM/From/To 排序，
// 与 docs/v0.1/12 §3.1 的 79 边口径一致）。
func AllEdges() []Edge {
	var out []Edge
	for _, info := range AllFSMs() {
		states := info.FSM.States()
		sort.Strings(states)
		for _, from := range states {
			for _, to := range info.FSM.Next(from) {
				out = append(out, Edge{FSM: info.Name, From: from, To: to})
			}
		}
	}
	return out
}

// FSM —— 表驱动状态机通用骨架（图谱四规则：状态机显式化，单一事实源）。
// 每个领域 FSM 用一张迁移表表达；非法迁移返回 ErrIllegalTransition。
type FSM struct {
	name  string
	table map[string][]string
	entry string // 初始状态（空串表示无默认入口，由外部指定）
}

// NewFSM 构造状态机。transitions 为 [from][to] 的迁移表；entry 为初始状态（可为空）。
func NewFSM(name, entry string, transitions map[string][]string) *FSM {
	return &FSM{name: name, table: transitions, entry: entry}
}

// ErrIllegalTransition 表示 from → to 不在迁移表中。
type ErrIllegalTransition struct {
	FSM  string
	From string
	To   string
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("%s: illegal transition %s -> %s", e.FSM, e.From, e.To)
}

// IsIllegalTransition 判断错误是否为非法迁移。
func IsIllegalTransition(err error) bool {
	_, ok := err.(*ErrIllegalTransition)
	return ok
}

// Entry 返回初始状态。
func (f *FSM) Entry() string { return f.entry }

// Can 判断 from → to 是否合法。
func (f *FSM) Can(from, to string) bool {
	for _, t := range f.table[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Validate 校验迁移；非法返回 ErrIllegalTransition。
func (f *FSM) Validate(from, to string) error {
	if f.Can(from, to) {
		return nil
	}
	return &ErrIllegalTransition{FSM: f.name, From: from, To: to}
}

// Next 返回 from 状态的全部合法后继（去重，含自身循环）。
func (f *FSM) Next(from string) []string {
	seen := map[string]bool{}
	var out []string
	for _, to := range f.table[from] {
		if !seen[to] {
			seen[to] = true
			out = append(out, to)
		}
	}
	return out
}

// States 返回全部已知状态（表驱动穷举测试用）。
func (f *FSM) States() []string {
	seen := map[string]bool{}
	var out []string
	if f.entry != "" {
		seen[f.entry] = true
		out = append(out, f.entry)
	}
	for from, tos := range f.table {
		if !seen[from] {
			seen[from] = true
			out = append(out, from)
		}
		for _, to := range tos {
			if !seen[to] {
				seen[to] = true
				out = append(out, to)
			}
		}
	}
	return out
}
