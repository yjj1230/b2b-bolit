//go:build !deferred

package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/wecom"
)

// handleMockMessage 是测试注入入口；生产消息必须经过已验签的企微通道。
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
	if req.TenantID == "" {
		req.TenantID = "chang-an"
	}
	result, err := s.processAgentMessage(r.Context(), req.TenantID, req.GroupID, req.MsgID, req.SenderID, req.Text)
	if err != nil {
		writeErr(w, err.Error())
		return
	}
	if result.Created && (result.PreOrder.State == domain.StatePrePendingConfirm || result.PreOrder.State == domain.StatePreAmbiguous) {
		if err := s.startAgentPreOrderFlow(r.Context(), req.TenantID, result.PreOrder.ID, req.SenderID, result.PreOrder.Candidates); err != nil {
			writeErr(w, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{
		"pre_order_id": result.PreOrder.ID, "session_id": result.Conversation.SessionID,
		"state": result.PreOrder.State, "created": result.Created, "draft_version": result.Draft.DraftVersion,
		"candidates": string(result.PreOrder.Candidates),
	})
}

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
		result, err := s.processAgentMessage(ctx, msg.TenantID, msg.GroupID, msg.MsgID, msg.SenderID, msg.Text)
		if err != nil {
			return err
		}
		if result.Created && (result.PreOrder.State == domain.StatePrePendingConfirm || result.PreOrder.State == domain.StatePreAmbiguous) {
			if err := s.startAgentPreOrderFlow(ctx, msg.TenantID, result.PreOrder.ID, msg.SenderID, result.PreOrder.Candidates); err != nil {
				return err
			}
		}
	}
	return s.store.AdvanceArchiveCursor(ctx, tenantID, nextSeq)
}

func (s *wecomService) startAgentPreOrderFlow(ctx context.Context, tenantID, preOrderID, senderID string, candidates []byte) error {
	if s.starter != nil {
		_, err := s.starter.StartPreOrderWorkflow(ctx, tenantID, preOrderID, senderID)
		return err
	}
	return startPreOrderFlow(ctx, s.store, s.gw, tenantID, preOrderID, candidates)
}

func startPreOrderFlow(ctx context.Context, store *repo.Repo, gw wecom.Gateway, tenantID, preOrderID string, candidates []byte) error {
	tx, err := store.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := store.UpdatePreOrderState(ctx, tx, tenantID, preOrderID, domain.StatePreCaptured, domain.StatePrePendingConfirm,
		"IntentParsed", "规则解析结果待人工确认", candidates, ""); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return wecom.NewNotifier(gw).NotifyPreOrderConfirm(ctx, tenantID, preOrderID, candidates)
}
