// cmd/worker —— Temporal Worker（docs/02-architecture.html §2.1）
package main

import (
	"context"
	"log"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/shadowguard"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

func main() {
	cfg := config.Load()

	// SH-13/A-01：影子模式启动守卫六项 fail-fast（DSN/namespace/queue/WeCom/bridge/SpiceDB）。
	if cfg.ShadowMode {
		if err := shadowguard.Validate(cfg); err != nil {
			log.Fatalf("shadow guard: %v", err)
		}
	}

	store, err := repo.New(context.Background(), cfg.PGDSN)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer store.Close()

	// 订单数据实体（products/customers/purchase-contracts）经 pgAPI 访问；worker 必须配置。
	data, err := orderdata.NewHTTPProviderWithTimeout(cfg.PGAPIBaseURL, cfg.PGAPIAPIKey, cfg.PGAPITimeout)
	if err != nil {
		log.Fatalf("orderdata pgapi: %v", err)
	}
	log.Printf("orderdata=pgapi base=%s", cfg.PGAPIBaseURL)

	checker, err := authz.NewSpiceDB(cfg.SpiceDBAddr, cfg.SpiceDBKey)
	if err != nil {
		log.Fatalf("connect spicedb: %v", err)
	}

	// AUTH-03 收敛：v0.1 启用 Outbox 投影器（M4 原为「不启用」，但生产 SpiceDB 对象级判定
	// 需要 prestock/shipment 实例关系；投影器消费 outbox_events → Touch SpiceDB 关系元组）。
	// 关系基线（tenant#ops、warehouse#staff 等）仍由 scripts/seedrel 预置。
	proj := authz.NewProjector(store, checker)
	projCtx, stopProj := context.WithCancel(context.Background())
	defer stopProj()
	if _, err := proj.RunOnce(projCtx, 100); err != nil {
		log.Printf("outbox projector initial drain: %v", err)
	}
	go proj.Run(projCtx, 5*time.Second)
	log.Printf("outbox projector enabled interval=5s")

	notifier, err := wecom.NewGateway(cfg)
	if err != nil {
		log.Fatalf("wecom gateway: %v", err)
	}

	c, err := client.Dial(client.Options{HostPort: cfg.TemporalAddr, Namespace: cfg.TemporalNS})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, cfg.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.OrderWorkflow)
	w.RegisterWorkflow(workflows.PreOrderWorkflow)
	w.RegisterActivity(workflows.NewActivitiesWithData(store, wecom.NewNotifier(notifier), checker, data))

	// Agent 正式订单通过独立 workflow outbox 最终一致地启动，避免在数据库事务内直调 Temporal。
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	defer stopDispatch()
	dispatcher := &workflows.WorkflowOutboxDispatcher{
		Store: store, Client: c, TaskQueue: cfg.TaskQueue,
		PollInterval: 2 * time.Second, Lease: 30 * time.Second, MaxAttempts: 8,
	}
	go func() {
		if err := dispatcher.Run(dispatchCtx); err != nil && dispatchCtx.Err() == nil {
			log.Printf("workflow outbox dispatcher stopped: %v", err)
		}
	}()
	log.Printf("workflow outbox dispatcher enabled interval=2s")

	log.Printf("worker listening on %s queue=%s shadow=%v", cfg.TemporalAddr, cfg.TaskQueue, cfg.ShadowMode)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker run: %v", err)
	}
}
