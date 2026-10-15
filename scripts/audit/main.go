// scripts/audit —— 审计链回放（L3-05：事件链断链/合法迁移/终态分录检查）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "PG DSN")
	flag.Parse()

	ctx := context.Background()
	r, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer r.Close()

	orders, err := r.ListOrders(ctx, "", "", 1000)
	if err != nil {
		log.Fatalf("list orders: %v", err)
	}
	fail := 0
	for _, ord := range orders {
		events, err := r.ListOrderEvents(ctx, ord.TenantID, ord.ID, 1000)
		if err != nil {
			log.Fatalf("events: %v", err)
		}
		if len(events) == 0 {
			fmt.Printf("[audit] ORDER %s/%s: EMPTY EVENT CHAIN\n", ord.TenantID, ord.ID)
			fail++
			continue
		}
		// 断链检查：事件迁移必须首尾相接（from_state[i+1] == to_state[i]），
		// 且每一步迁移对 domain FSM 合法（append-only 链即合法迁移序列）。
		prevTo := ""
		for i, ev := range events {
			if ev.FromState != nil && *ev.FromState != "" && prevTo != "" && *ev.FromState != prevTo {
				fmt.Printf("[audit] ORDER %s/%s event %d: chain break %s -> %s (prev to=%s)\n",
					ord.TenantID, ord.ID, i, *ev.FromState, deref(ev.ToState), prevTo)
				fail++
			}
			if ev.FromState != nil && ev.ToState != nil && *ev.FromState != "" && *ev.ToState != "" {
				if err := validateTransition(*ev.FromState, *ev.ToState); err != nil {
					fmt.Printf("[audit] ORDER %s/%s event %d: illegal transition %s -> %s\n",
						ord.TenantID, ord.ID, i, *ev.FromState, *ev.ToState)
					fail++
				}
			}
			if ev.ToState != nil {
				prevTo = *ev.ToState
			}
		}
		// 终态分录检查：CLOSED/REJECTED 订单必须有终态事件
		if domain.IsTerminal(ord.State) {
			last := events[len(events)-1]
			if last.ToState == nil || *last.ToState != ord.State {
				fmt.Printf("[audit] ORDER %s/%s: terminal state %s but last event to=%s\n",
					ord.TenantID, ord.ID, ord.State, deref(last.ToState))
				fail++
			}
		}
	}
	fmt.Printf("[audit] orders=%d failures=%d\n", len(orders), fail)
	if fail > 0 {
		log.Fatalf("audit FAILED: %d issue(s)", fail)
	}
	fmt.Println("[audit] PASS")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func validateTransition(from, to string) error {
	switch domain.Stage(from) {
	case "ORD":
		return domain.OrdFSM.Validate(from, to)
	case "STK":
		return domain.StkFSM.Validate(from, to)
	case "SHP":
		return domain.ShpFSM.Validate(from, to)
	case "INV":
		return domain.InvFSM.Validate(from, to)
	case "PRE":
		return domain.PreOrderFSM.Validate(from, to)
	}
	return nil
}
