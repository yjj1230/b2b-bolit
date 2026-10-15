//go:build deferred

package main

// ── 暂缓段（07 §3.0：「基于意图识别的企微自动下单」暂缓实现、暂缓测试）──
//
// 默认构建（!deferred）不编译本文件：registerRoutes 由 routes.go 提供（仅回调）；
// -tags deferred 构建恢复：mock 群消息注入（消息 → 意图解析 → 自动建预订单）、
// 会话存档拉取循环（seq checkpoint / 两段解密 / 5 天窗口守护）。
//
// 恢复条件（全部满足方可解除暂缓，见 docs/07 §3.0）：
//   1) 真实企业会话存档（S2）或等效消息通道开通并完成合规决策门（07 §2.4）；
//   2) LLM 供应商与密钥管理（KMS）就绪；
//   3) SKU 标准型号映射表确定维护人与来源；
//   4) 恢复时同步恢复 docs/07 §2-§3 测试矩阵与门禁条目。
import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

// registerRoutes deferred 构建路由：回调 + 健康检查 + 暂缓段（mock 消息注入 / 存档拉取）。
func registerRoutes(mux *http.ServeMux, svc *wecomService, gw wecom.Gateway) {
	mux.HandleFunc("GET /wecom/callback/{tenant}", svc.handleCallbackVerify)
	mux.HandleFunc("POST /wecom/callback/{tenant}", svc.handleCallback)
	mux.HandleFunc("GET /wecom/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "provider": gw.Name()})
	})
	// 暂缓段路由（07 §3.0）：mock 通道群消息注入（S2 模拟；HTTP API 测试模式入口）
	mux.HandleFunc("POST /wecom/mock/message", svc.handleMockMessage)
	// official 通道：会话存档拉取循环（seq checkpoint 原子持久化，07 §2.3）
	if svc.agentEnabled && svc.ingressEnabled && gw.Name() == "official" {
		go svc.archiveLoop(context.Background())
	}
}

// handleMockMessage mock 通道入站：消息 → 意图解析 → 预订单 → 启动 PreOrderWorkflow。
func (s *wecomService) handleMockMessage(w http.ResponseWriter, r *http.Request) {
	if !s.agentEnabled || s.parser == nil {
		writeJSONErr(w, http.StatusNotFound, "agent ordering disabled")
		return
	}
	var req struct {
		TenantID string `json:"tenant_id"`
		GroupID  string `json:"group_id"`
		MsgID    string `json:"msg_id"`
		SenderID string `json:"sender_id"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err.Error())
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = "chang-an"
	}
	// 消息幂等（CQ-11）：(tenant, group, msgid) 唯一
	key, err := wecomPreKey(tenantID, req.GroupID, req.MsgID)
	if err != nil {
		writeErr(w, err.Error())
		return
	}
	candidates, err := s.parser.Parse(r.Context(), tenantID, req.GroupID, req.MsgID, req.Text)
	if err != nil {
		writeErr(w, "intent parse: "+err.Error())
		return
	}
	po, created, err := s.store.CreatePreOrder(r.Context(), repo.NewPreOrder{
		TenantID: tenantID, GroupID: req.GroupID, MsgID: req.MsgID,
		Candidates: candidates, IdempotencyKey: key,
	})
	if err != nil {
		writeErr(w, err.Error())
		return
	}
	// 触发意图识别 → 确认卡片（S2；基线直接在服务侧推进到 PENDING_CONFIRM 前）
	if created {
		if _, err := s.store.CreatePreOrderDraftVersion(r.Context(), repo.NewPreOrderDraftVersion{
			TenantID: tenantID, PreOrderID: po.ID, SourceMsgID: req.MsgID,
			SourceMsgHash: repo.HashPayload([]byte(req.Text)), Candidate: candidates,
			PolicyStatus: "PENDING", Evidence: []byte("[]"), Quote: []byte("{}"),
			ModelName: "parser", ModelVersion: "configured", PromptVersion: "v1",
		}); err != nil {
			writeErr(w, "create draft: "+err.Error())
			return
		}
		if err := s.startAgentPreOrderFlow(r.Context(), tenantID, po.ID, req.SenderID, po.Candidates); err != nil {
			writeErr(w, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{
		"pre_order_id": po.ID, "state": po.State, "created": created, "candidates": string(candidates),
	})
}

// archiveLoop 会话存档拉取循环（official；原子 checkpoint）。
func (s *wecomService) archiveLoop(ctx context.Context) {
	for {
		if err := s.pullOnce(ctx); err != nil {
			log.Printf("archive pull: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// pullOnce 单次拉取：游标 → Pull → 逐条建预订单 → checkpoint。
func (s *wecomService) pullOnce(ctx context.Context) error {
	tenantID := "chang-an"
	seq, err := s.store.GetArchiveCursor(ctx, tenantID)
	if err != nil {
		return err
	}
	msgs, nextSeq, err := s.gw.Pull(ctx, seq, 100)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if msg.TenantID == "" {
			msg.TenantID = tenantID
		}
		key, err := wecomPreKey(msg.TenantID, msg.GroupID, msg.MsgID)
		if err != nil {
			continue
		}
		candidates, err := s.parser.Parse(ctx, msg.TenantID, msg.GroupID, msg.MsgID, msg.Text)
		if err != nil {
			continue
		}
		createdPO, created, err := s.store.CreatePreOrder(ctx, repo.NewPreOrder{
			TenantID: msg.TenantID, GroupID: msg.GroupID, MsgID: msg.MsgID,
			Candidates: candidates, IdempotencyKey: key,
		})
		if err != nil {
			return err
		}
		if created {
			if _, err := s.store.CreatePreOrderDraftVersion(ctx, repo.NewPreOrderDraftVersion{
				TenantID: msg.TenantID, PreOrderID: createdPO.ID, SourceMsgID: msg.MsgID,
				SourceMsgHash: repo.HashPayload([]byte(msg.Text)), Candidate: candidates,
				PolicyStatus: "PENDING", Evidence: []byte("[]"), Quote: []byte("{}"),
				ModelName: "parser", ModelVersion: "configured", PromptVersion: "v1",
			}); err != nil {
				return err
			}
			if err := s.startAgentPreOrderFlow(ctx, msg.TenantID, createdPO.ID, msg.SenderID, candidates); err != nil {
				return err
			}
		}
	}
	// 原子 checkpoint（L1-14；5 天窗口防线）
	return s.store.AdvanceArchiveCursor(ctx, tenantID, nextSeq)
}

func (s *wecomService) startAgentPreOrderFlow(ctx context.Context, tenantID, preOrderID, senderID string, candidates []byte) error {
	if s.starter != nil {
		_, err := s.starter.StartPreOrderWorkflow(ctx, tenantID, preOrderID, senderID)
		return err
	}
	return startPreOrderFlow(ctx, s.store, s.gw, tenantID, preOrderID, candidates)
}

// startPreOrderFlow 预订单 → 意图解析 → 确认卡片（S2 管线；worker 未跑时由服务侧推进）。
func startPreOrderFlow(ctx context.Context, store *repo.Repo, gw wecom.Gateway, tenantID, preOrderID string, candidates []byte) error {
	// 基线：服务侧直接推进 CAPTURED → INTENT_PARSING → PENDING_CONFIRM（确定性管线）；
	// PreOrderWorkflow 完整编排在 worker/本地 runner 中运行。
	tx, err := store.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := store.UpdatePreOrderState(ctx, tx, tenantID, preOrderID, "PRE_CAPTURED", "PRE_PENDING_CONFIRM",
		"IntentParsed", "规则解析高置信", candidates, ""); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	err = wecom.NewNotifier(gw).NotifyPreOrderConfirm(ctx, tenantID, preOrderID, candidates)
	return err
}
