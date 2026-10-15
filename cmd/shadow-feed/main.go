// cmd/shadow-feed —— 影子轨事件 → Signal 翻译投递（docs/v0.1/12-shadow-full-flow.html §5 步骤 2-6）
//
// 每轮：读 NEW 源事件 → shadow.PlanFeedWithStarted（白名单映射 + 幂等 + 时间序 + 分歧捕获）→
// 建影子订单 + StartWorkflow（Shadow=true）→ 按计划投递 Signal → shadow_signal_log 台账。
//
// 零猜测：未知动作 → UNMAPPED_EVENT 分歧不投递；同 (flow, event) 幂等；同流按摄入序串行。
// 影子模式：Timer 全不调度；shadow-inject 由本命令按映射投递（kind=receipt-exception）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/shadow"
	"b2b_pilot_v3/internal/shadowguard"
	"b2b_pilot_v3/internal/workflows"
)

func main() {
	cfgPath := flag.String("config", "", "事件映射配置 JSON（默认取 SHADOW_FEED_CONFIG）")
	dsn := flag.String("dsn", "", "影子库 DSN（默认取 B2B_PG_DSN）")
	interval := flag.Duration("interval", 0, "投递周期（如 1m）；0 = 只跑一轮")
	stateFile := flag.String("state-file", "", "每轮成功后的探针文件")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := config.Load()
	if *cfgPath == "" {
		*cfgPath = cfg.ShadowFeedConfig
	}
	if *dsn == "" {
		*dsn = cfg.PGDSN
	}
	cfg.PGDSN = *dsn
	if err := shadowguard.Validate(cfg); err != nil {
		log.Fatalf("shadow guard: %v", err)
	}

	ctx := context.Background()
	sstore, err := shadow.Open(ctx, *dsn)
	if err != nil {
		log.Fatalf("open shadow pg: %v", err)
	}
	defer sstore.Close()
	rstore, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("open repo pg: %v", err)
	}
	defer rstore.Close()

	feedCfg, err := shadow.LoadFeedConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load feed config: %v", err)
	}
	feedDigest := fileDigest(*cfgPath)

	c, err := client.Dial(client.Options{HostPort: cfg.TemporalAddr, Namespace: cfg.TemporalNS})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()
	starter := workflows.NewTemporalStarter(c, rstore).ConfigureQueue(cfg.TaskQueue, workflows.ShadowWorkflowIDPrefix)
	// 影子租户审批人路由（008 种子 u-shadow-*；B2B_ASSIGNEE_* 注入，默认 A 轨 u-* 不适用影子库）。
	starter.SetAssignees(workflows.AssigneeConfig{
		Supervisor: cfg.AssigneeSupervisor, Chairman: cfg.AssigneeChairman,
		OP: cfg.AssigneeOP, Accounting: cfg.AssigneeAccounting, Sales: cfg.AssigneeSales,
		WarehousePrefix: cfg.AssigneeWarehousePrefix, WarehouseFallback: cfg.AssigneeWarehouseDefault,
	})

	run := func() {
		runID := fmt.Sprintf("feed-%s", time.Now().UTC().Format("20060102T150405.000"))
		if err := sstore.BeginRun(ctx, runID, "", feedDigest, "shadow-feed"); err != nil {
			log.Printf("begin run: %v", err)
			return
		}
		if err := runOnce(ctx, sstore, rstore, starter, feedCfg, runID); err != nil {
			_ = sstore.FinishRun(ctx, runID, "FAILED", err.Error())
			log.Printf("shadow feed run %s failed: %v", runID, err)
			return
		}
		if err := sstore.FinishRun(ctx, runID, "COMPLETED", ""); err != nil {
			log.Printf("finish run: %v", err)
		}
		touchStateFile(*stateFile)
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

// runOnce 一轮投递。
func runOnce(ctx context.Context, ss *shadow.Store, rs *repo.Repo,
	starter *workflows.TemporalStarter, feedCfg *shadow.FeedConfig, runID string) error {
	events, err := ss.ListSourceEvents(ctx, feedCfg.TenantID, "NEW", 10000)
	if err != nil {
		return fmt.Errorf("list source events: %w", err)
	}
	started, err := ss.ListStartedFlowIDs(ctx, feedCfg.TenantID)
	if err != nil {
		return fmt.Errorf("list started flows: %w", err)
	}
	plan := shadow.PlanFeedWithStarted(feedCfg, events, started)
	for _, d := range plan.Divergences {
		if _, _, err := ss.RecordDivergence(ctx, runID, d); err != nil {
			log.Printf("record divergence: %v", err)
		}
	}
	// 不投递事件置 SKIPPED：修复映射/链断裂后经 shadow-replay 全量重放，不每轮重复滚分歧。
	for _, id := range plan.SkippedEventIDs {
		if err := ss.MarkSourceEventState(ctx, id, "SKIPPED"); err != nil {
			log.Printf("mark event %d skipped: %v", id, err)
		}
	}

	// 事件 → Signal 映射表（12 §6）：已冻结配置驱动的权威映射；
	// 这里把翻译后的通用 payload 收敛为 workflow 的强类型 Signal（类型错误 = 启动即失败，不静默）。
	type eventWorkflow struct {
		eventID            int64
		flowID, workflowID string
	}
	wfByFlow := map[string]string{}
	for _, f := range started {
		if wid, err := ss.LatestWorkflowIDForFlow(ctx, feedCfg.TenantID, f); err == nil && wid != "" {
			wfByFlow[f] = wid
		}
	}
	var pending []eventWorkflow

	for i := range plan.Steps {
		step := &plan.Steps[i]
		if step.Kind == shadow.StepStartOrder {
			ord, wid, err := startShadowOrder(ctx, rs, starter, feedCfg, step.Event)
			if err != nil {
				log.Printf("flow %s start order: %v (retry next round)", step.Event.LegacyFlowID, err)
				continue
			}
			step.OrderID = ord.ID
			step.WorkflowID = wid
			wfByFlow[step.Event.LegacyFlowID] = wid
			if logID, _, err := ss.UpsertSignalLog(ctx, shadow.SignalLogEntry{
				SourceEventID: step.Event.ID, Signal: "__start__", Payload: step.Delivery.Payload, WorkflowID: wid,
			}); err == nil {
				_ = ss.MarkSignalDelivered(ctx, logID)
			}
			_ = ss.MarkSourceEventState(ctx, step.Event.ID, "FED")
			continue
		}

		wid := wfByFlow[step.Event.LegacyFlowID]
		if wid == "" {
			wid, _ = ss.LatestWorkflowIDForFlow(ctx, feedCfg.TenantID, step.Event.LegacyFlowID)
		}
		if wid == "" {
			// 事件在计划里但 workflow 尚不可投递（本轮建单失败等）：留 NEW 下轮重试。
			pending = append(pending, eventWorkflow{step.Event.ID, step.Event.LegacyFlowID, ""})
			continue
		}
		step.WorkflowID = wid

		payload, err := shadow.TypedSignal(step.Delivery.Signal, step.Delivery.Payload)
		if err != nil {
			d := shadow.NewDivergence(shadow.KindMapping, "", "MISSING_FIELD", step.Event.LegacyFlowID,
				fmt.Sprintf("signal %s payload invalid: %v", step.Delivery.Signal, err))
			_, _, _ = ss.RecordDivergence(ctx, runID, d)
			_ = ss.MarkSourceEventState(ctx, step.Event.ID, "SKIPPED")
			continue
		}
		logID, _, err := ss.UpsertSignalLog(ctx, shadow.SignalLogEntry{
			SourceEventID: step.Event.ID, Signal: step.Delivery.Signal,
			Payload: step.Delivery.Payload, WorkflowID: wid,
		})
		if err != nil {
			return fmt.Errorf("upsert signal log: %w", err)
		}
		if err := starter.SignalWorkflow(ctx, wid, step.Delivery.Signal, payload); err != nil {
			_ = ss.MarkSignalFailed(ctx, logID, err.Error())
			d := shadow.NewDivergence(shadow.KindLegacyAnomaly, "", shadow.ReasonChainBreak, step.Event.LegacyFlowID,
				fmt.Sprintf("signal %s to workflow %s failed: %v", step.Delivery.Signal, wid, err))
			_, _, _ = ss.RecordDivergence(ctx, runID, d)
			log.Printf("flow %s signal %s failed: %v (NEW retained for retry)", step.Event.LegacyFlowID, step.Delivery.Signal, err)
			continue
		}
		_ = ss.MarkSignalDelivered(ctx, logID)
		_ = ss.MarkSourceEventState(ctx, step.Event.ID, "FED")
		wfByFlow[step.Event.LegacyFlowID] = wid
	}
	log.Printf("feed plan steps=%d divergences=%d events=%d pending=%d", len(plan.Steps), len(plan.Divergences), len(events), len(pending))
	return nil
}

// startShadowOrder 影子建单（12 §5 步骤 2）：idempotency 搬源键 + 幂等 StartWorkflow。
func startShadowOrder(ctx context.Context, rs *repo.Repo, starter *workflows.TemporalStarter,
	cfg *shadow.FeedConfig, ev shadow.SourceEvent) (*repo.Order, string, error) {
	// 影子建单：幂等键搬源键（flow_id），并保持订单键规范 order:<tenant>:<source>:<uuid>
	//（source=quick；uuid=shadow-<flow> → 与 A 轨快速下单 ID 空间/语义一致）。
	idemKey, err := domain.OrderKey(cfg.TenantID, "quick", "shadow-"+ev.LegacyFlowID)
	if err != nil {
		return nil, "", fmt.Errorf("shadow idempotency key: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"flow_id": ev.LegacyFlowID, "source": "shadow-ingest"})
	flowID := ev.LegacyFlowID
	// 字段名兼容：真实 JSONL 是 customer_record_id / payment_terms / supplier / unit_price，
	// 旧配置假设 customer_id / payment_term / supplier_channel / unit_price_cents。
	customerID := firstNonEmpty(
		shadow.StrOf(ev.Payload, "customer_record_id", ""),
		shadow.StrOf(ev.Payload, "customer_id", ""),
		"shadow-cust",
	)
	paymentTerm := firstNonEmpty(
		shadow.StrOf(ev.Payload, "payment_terms", ""),
		shadow.StrOf(ev.Payload, "payment_term", ""),
		cfg.DefaultPaymentTerm,
	)
	if paymentTerm != "款到" && paymentTerm != "非款到" {
		// orders.payment_term CHECK 仅允许两值；旧系统异名账期回退配置默认值并留源在 payload
		paymentTerm = cfg.DefaultPaymentTerm
	}
	supplier := firstNonEmpty(
		shadow.StrOf(ev.Payload, "supplier", ""),
		shadow.StrOf(ev.Payload, "supplier_channel", ""),
		cfg.DefaultSupplierChannel,
	)
	qty := shadow.I64Of(ev.Payload, "quantity", 1)
	price := shadow.I64Of(ev.Payload, "unit_price", 0)
	if price <= 0 {
		price = shadow.I64Of(ev.Payload, "unit_price_cents", cfg.DefaultUnitPriceCents)
	}
	productID := shadow.StrOf(ev.Payload, "product_id", cfg.DefaultProductID)
	ord, created, err := rs.CreateOrder(ctx, repo.NewOrder{
		TenantID: cfg.TenantID, CustomerID: customerID,
		PaymentTerm: paymentTerm, SupplierChannel: supplier,
		IdempotencyKey: idemKey, Payload: payload,
		SubmissionMode: "shadow", LegacyFlowID: &flowID,
		Items: []repo.NewOrderItem{{
			ID: "shadow-item-" + flowID, ProductID: productID,
			Quantity: qty, UnitPriceCents: price,
		}},
	})
	if err != nil {
		return nil, "", err
	}
	wid, err := starter.StartShadowOrderWorkflow(ctx, cfg.TenantID, ord.ID)
	if err != nil {
		return nil, "", fmt.Errorf("start shadow workflow (order created=%v): %w", created, err)
	}
	return ord, wid, nil
}

// firstNonEmpty 返回首个非空字符串（兼容真实 JSONL 与配置假设字段名差异）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func fileDigest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:4])
}

func touchStateFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		log.Printf("write state file: %v", err)
	}
}
