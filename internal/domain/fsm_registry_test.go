package domain

import (
	"testing"
)

// 影子轨 S-01 设计口径：六台状态机；多轮下单新增收集和过期状态。
// AllFSMs/AllEdges 是 shadow 覆盖率/回放器的单一事实源，必须在本包 100% 覆盖。
func TestFSMRegistryInventory(t *testing.T) {
	infos := AllFSMs()
	if len(infos) != 6 {
		t.Fatalf("fsm count = %d, want 6", len(infos))
	}
	states := 0
	for _, info := range infos {
		if info.Name == "" || info.FSM == nil {
			t.Fatalf("invalid fsm info: %+v", info)
		}
		states += len(info.FSM.States())
	}
	if states != 54 {
		t.Fatalf("state count = %d, want 54", states)
	}
	edges := AllEdges()
	if len(edges) != 93 {
		t.Fatalf("edge count = %d, want 93", len(edges))
	}
	for _, e := range edges {
		var fsm *FSM
		for _, info := range infos {
			if info.Name == e.FSM {
				fsm = info.FSM
				break
			}
		}
		if fsm == nil {
			t.Fatalf("edge references unknown fsm %q", e.FSM)
		}
		if err := fsm.Validate(e.From, e.To); err != nil {
			t.Fatalf("registry edge %s %s->%s invalid: %v", e.FSM, e.From, e.To, err)
		}
	}
	// 确定性：两次枚举同序（覆盖率矩阵可复算）
	a, b := AllEdges(), AllEdges()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("edge enumeration not deterministic at %d", i)
		}
	}
}

func TestFSMRegistryNamesMatchDesign(t *testing.T) {
	want := []string{"ORD", "STK", "SHP", "INV", "PRE", "Gatekeeper"}
	infos := AllFSMs()
	for i, name := range want {
		if infos[i].Name != name {
			t.Fatalf("fsm[%d] = %q, want %q", i, infos[i].Name, name)
		}
	}
}
