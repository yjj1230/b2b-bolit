package shadow

import (
	"fmt"
	"sort"
	"time"
)

// ── shadow-feed 投递计划（12 §5 步骤 2-6 的可执行纯逻辑）──
//
// PlanFeed 把「翻译 + 幂等 + 时间序 + 分歧捕获」做成确定性纯函数：
// 同输入两次 → 完全相同计划（S-03/S-05 重放口径）；cmd/shadow-feed 只负责执行计划。

// FeedStepKind 计划步骤类型。
type FeedStepKind string

const (
	StepStartOrder FeedStepKind = "start"  // 建影子订单 + StartWorkflow
	StepSignal     FeedStepKind = "signal" // 幂等投递 Signal
)

// FeedStep 一步可执行计划。
type FeedStep struct {
	Kind       FeedStepKind
	Event      SourceEvent
	Delivery   Delivery
	OrderID    string // 建单步骤回填（执行时）
	WorkflowID string // 建单步骤回填（执行时）
}

// FeedPlan 确定性投递计划。
type FeedPlan struct {
	Steps       []FeedStep
	Divergences []Divergence
	// SkippedEventIDs 不投递且不重试的事件（未映射/缺字段/链断裂/重复建单）：
	// feed 将其置 SKIPPED；修复映射后经 shadow-replay 全量重放，不重复滚分歧。
	SkippedEventIDs []int64
}

// PlanFeed S-05：给定时间序源事件，生成投递计划（无预启动流）。
func PlanFeed(cfg *FeedConfig, events []SourceEvent) FeedPlan {
	return PlanFeedWithStarted(cfg, events, nil)
}

// PlanFeedWithStarted 生成投递计划；started 为已有影子订单的流（增量轮次：
// 建单事件在上轮已投递，本轮信号直接接续，不误判为链断裂）。
//
// 规则：
//   - 按 (flow, 摄入序 id) 稳定排序（忠实回放摄入序；时间倒挂显式记录 DATA_GAP）；
//   - 每 flow 首个命中 Start 映射的事件建单；之前/缺失建单事件的流记 DATA_GAP；
//   - (flow_id, source_event_id) 幂等：重复投递只记录一次（静默跳过，不算分歧）；
//   - 未知动作 → UNMAPPED_EVENT 分歧（MAPPING，P2）；缺必填字段 → MAPPING 分歧；
//   - 同 flow 时间倒挂 → DATA_GAP 分歧（仍按事件序推进，不猜测）。
func PlanFeedWithStarted(cfg *FeedConfig, events []SourceEvent, started []string) FeedPlan {
	plan := FeedPlan{}
	startedFlows := map[string]bool{}
	for _, f := range started {
		startedFlows[f] = true
	}
	sorted := append([]SourceEvent(nil), events...)
	// 保持摄入序（id 递增），仅按 flow 聚组：事件时间倒挂显式记录 DATA_GAP，
	// 而不是静默重排掩盖源数据异常（重放 = 忠实回放摄入序）。
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].LegacyFlowID != sorted[j].LegacyFlowID {
			return sorted[i].LegacyFlowID < sorted[j].LegacyFlowID
		}
		return sorted[i].ID < sorted[j].ID
	})

	seen := map[string]bool{} // (flow, event_id) 幂等
	lastTime := map[string]time.Time{}
	pending := map[string][]FeedStep{} // 建单事件之前的同流事件：先建单再投递

	for _, ev := range sorted {
		key := ev.LegacyFlowID + "|" + ev.LegacyEventID
		if seen[key] {
			continue // 幂等：重复投递只记录一次
		}
		seen[key] = true

		if lt, ok := lastTime[ev.LegacyFlowID]; ok && ev.OccurredAt.Before(lt) {
			d := NewDivergence(KindDataGap, "", ReasonTimeInversion, ev.LegacyFlowID,
				fmt.Sprintf("event %s occurred_at %s earlier than previous %s",
					ev.LegacyEventID, ev.OccurredAt.Format(time.RFC3339), lt.Format(time.RFC3339)))
			d.SourceEventID = ev.ID
			plan.Divergences = append(plan.Divergences, d)
		}
		if !ev.OccurredAt.IsZero() {
			if lt, ok := lastTime[ev.LegacyFlowID]; !ok || ev.OccurredAt.After(lt) {
				lastTime[ev.LegacyFlowID] = ev.OccurredAt
			}
		}

		delivery, err := Translate(cfg, ev)
		switch {
		case err == nil:
		default:
			d := classifyTranslateError(ev, err)
			plan.Divergences = append(plan.Divergences, d)
			plan.SkippedEventIDs = append(plan.SkippedEventIDs, ev.ID)
			continue
		}

		step := FeedStep{Event: ev, Delivery: delivery}
		if delivery.Signal == "ignore" {
			// 已知但不驱动新系统状态机的旧系统状态：不投递、不写分歧、置 SKIPPED。
			plan.SkippedEventIDs = append(plan.SkippedEventIDs, ev.ID)
			continue
		}
		if delivery.Start {
			if startedFlows[ev.LegacyFlowID] {
				// 同流第二个建单事件：幂等忽略（真相在首个事件）；置 SKIPPED 防每轮重进
				plan.SkippedEventIDs = append(plan.SkippedEventIDs, ev.ID)
				continue
			}
			startedFlows[ev.LegacyFlowID] = true
			step.Kind = StepStartOrder
			plan.Steps = append(plan.Steps, step)
			// 建单事件之前已被排序的孤儿事件：补投（链序纠正，不静默丢事件）
			for _, prev := range pending[ev.LegacyFlowID] {
				plan.Steps = append(plan.Steps, prev)
			}
			delete(pending, ev.LegacyFlowID)
			continue
		}
		step.Kind = StepSignal
		if !startedFlows[ev.LegacyFlowID] {
			// 建单事件缺失/尚未出现：先挂起，建单后再投；若计划结束仍未建单 → DATA_GAP
			pending[ev.LegacyFlowID] = append(pending[ev.LegacyFlowID], step)
			continue
		}
		plan.Steps = append(plan.Steps, step)
	}

	// 流内只有信号事件而无建单事件 → 链断裂（LEGACY_ANOMALY），不投递。
	// 按 flow ID 排序输出，保证多次重放产生完全一致的分歧顺序（确定性）。
	flowIDs := make([]string, 0, len(pending))
	for f := range pending {
		flowIDs = append(flowIDs, f)
	}
	sort.Strings(flowIDs)
	for _, flowID := range flowIDs {
		for _, step := range pending[flowID] {
			d := NewDivergence(KindLegacyAnomaly, "", ReasonNoCreationEvent, step.Event.LegacyFlowID,
				fmt.Sprintf("event %s has no creation event in this run (chain break)", step.Event.LegacyEventID))
			d.SourceEventID = step.Event.ID
			plan.Divergences = append(plan.Divergences, d)
			plan.SkippedEventIDs = append(plan.SkippedEventIDs, step.Event.ID)
		}
	}
	return plan
}

func classifyTranslateError(ev SourceEvent, err error) Divergence {
	var d Divergence
	switch e := err.(type) {
	case *ErrUnmapped:
		d = NewDivergence(KindMapping, "", ReasonUnmappedEvent, ev.LegacyFlowID, e.Error())
	case *ErrMissingField:
		d = NewDivergence(KindMapping, "", ReasonMissingField, ev.LegacyFlowID, e.Error())
	default:
		d = NewDivergence(KindMapping, "", ReasonUnmappedEvent, ev.LegacyFlowID, err.Error())
	}
	d.SourceEventID = ev.ID
	return d
}
