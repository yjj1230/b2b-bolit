package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"b2b_pilot_v3/internal/agentorder"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

type processedAgentMessage struct {
	Inbound      *repo.InboundMessage
	Conversation *repo.OrderConversation
	PreOrder     *repo.PreOrder
	Draft        *repo.PreOrderDraft
	Created      bool
}

// processAgentMessage implements the message -> conversation -> draft boundary.
// The parser only supplies candidates; Merge and all persistence decisions are
// deterministic and happen after the message fact has been written.
func (s *wecomService) processAgentMessage(ctx context.Context, tenantID, groupID, msgID, senderID, text string) (*processedAgentMessage, error) {
	inbound, created, err := s.store.CreateInboundMessage(ctx, repo.NewInboundMessage{
		TenantID: tenantID, GroupID: groupID, MsgID: msgID, SenderID: senderID, RawText: text,
	})
	if err != nil {
		return nil, err
	}
	if !created && inbound.SessionID != "" {
		conv, err := s.store.GetOrderConversation(ctx, tenantID, inbound.SessionID)
		if err != nil {
			return nil, err
		}
		po, err := s.store.GetPreOrder(ctx, tenantID, conv.PreOrderID)
		if err != nil {
			return nil, err
		}
		draft, err := s.store.GetCurrentPreOrderDraft(ctx, tenantID, conv.PreOrderID)
		if err != nil && !repo.IsNotFound(err) {
			return nil, err
		}
		return &processedAgentMessage{Inbound: inbound, Conversation: conv, PreOrder: po, Draft: draft, Created: false}, nil
	}

	conv, err := s.store.FindOpenOrderConversation(ctx, tenantID, groupID, senderID)
	if err != nil {
		if !repo.IsNotFound(err) {
			return nil, err
		}
		conv, _, err = s.store.CreateOrderConversation(ctx, repo.NewOrderConversation{
			TenantID: tenantID, GroupID: groupID, SenderID: senderID, RootMsgID: msgID,
		})
		if err != nil {
			return nil, err
		}
	}

	rawCandidates, err := s.parser.Parse(ctx, tenantID, groupID, msgID, text)
	if err != nil {
		return nil, fmt.Errorf("intent parse: %w", err)
	}
	patch := patchFromMessage(rawCandidates, text)
	previous := agentorder.DraftSnapshot{}
	if conv.CurrentDraftVersion > 0 {
		previousDraft, err := s.store.GetPreOrderDraft(ctx, tenantID, conv.PreOrderID, conv.CurrentDraftVersion)
		if err != nil {
			return nil, err
		}
		if len(previousDraft.SnapshotJSON) > 0 && string(previousDraft.SnapshotJSON) != "{}" {
			_ = json.Unmarshal(previousDraft.SnapshotJSON, &previous)
		}
	}
	if previous.Fields == nil {
		previous.Fields = map[string]agentorder.FieldValue{}
	}
	result := agentorder.Merge(previous, patch, agentorder.MergePolicy{
		RequiredFields: []string{"product_sku", "quantity", "receive_address"},
	})
	snapshotJSON, _ := json.Marshal(result.Snapshot)
	patchJSON, _ := json.Marshal(result.Patch)
	diffJSON, _ := json.Marshal(map[string]any{"fields": result.Diff, "conflicts": result.Conflicts, "missing": result.Missing})
	candidateJSON, _ := json.Marshal(candidateFromSnapshot(result.Snapshot))
	draftStatus := "CURRENT"
	nextState := domain.StatePrePendingConfirm
	policyStatus := "READY_FOR_CONFIRMATION"
	if result.Status == agentorder.MergeStatusConflict {
		draftStatus = "CONFLICT"
		nextState = domain.StatePreAmbiguous
		policyStatus = "CONFLICT"
	} else if len(result.Missing) > 0 {
		nextState = domain.StatePreCollecting
		policyStatus = "COLLECTING"
	}
	draft, updatedConv, err := s.store.ApplyConversationDraft(ctx, repo.ConversationDraftInput{
		TenantID: tenantID, SessionID: conv.SessionID, InboundMessageID: inbound.ID,
		SourceMsgID: msgID, SourceMsgHash: repo.HashPayload([]byte(text)),
		Candidate: candidateJSON, SnapshotJSON: snapshotJSON, PatchJSON: patchJSON,
		Evidence: []byte("[]"), Diff: diffJSON, PolicyStatus: policyStatus,
		ModelName: "agent-parser", ModelVersion: "configured", PromptVersion: "v1",
		PolicyVersion: "agent-order-policy-v1", CreatedBy: senderID, Status: draftStatus,
		NextPreOrderState: nextState,
	})
	if err != nil {
		return nil, err
	}
	po, err := s.store.GetPreOrder(ctx, tenantID, updatedConv.PreOrderID)
	if err != nil {
		return nil, err
	}
	return &processedAgentMessage{Inbound: inbound, Conversation: updatedConv, PreOrder: po, Draft: draft, Created: true}, nil
}

func patchFromMessage(raw []byte, text string) agentorder.FieldPatch {
	patch := agentorder.FieldPatch{Fields: map[string]agentorder.PatchValue{}}
	var candidates []workflows.IntentCandidate
	if json.Unmarshal(raw, &candidates) == nil && len(candidates) > 0 {
		c := candidates[0]
		addPatch(patch.Fields, "product_sku", c.ProductSKU, text)
		if c.Quantity > 0 {
			addPatch(patch.Fields, "quantity", c.Quantity, text)
		}
		addPatch(patch.Fields, "receive_address", c.ReceiveAddress, text)
		addPatch(patch.Fields, "customer_id", c.CustomerID, text)
		addPatch(patch.Fields, "payment_term", c.PaymentTerm, text)
		addPatch(patch.Fields, "warehouse_id", c.WarehouseID, text)
	}
	if m := regexp.MustCompile("(?i)SKU-[A-Z0-9]+").FindString(text); m != "" {
		addPatch(patch.Fields, "product_sku", strings.ToUpper(m), text)
	}
	if m := regexp.MustCompile("([0-9]+)\\s*(件|台|个)").FindStringSubmatch(text); len(m) == 3 {
		if qty, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			addPatch(patch.Fields, "quantity", qty, text)
		}
	}
	if m := regexp.MustCompile("(?i)(?:送到|地址[:：]?)(.+)$").FindStringSubmatch(strings.TrimSpace(text)); len(m) == 2 {
		addPatch(patch.Fields, "receive_address", strings.TrimSpace(m[1]), text)
	}
	return patch
}

func addPatch(fields map[string]agentorder.PatchValue, name string, value any, evidence string) {
	if value == nil || value == "" {
		return
	}
	text := strings.ToLower(evidence)
	explicit := strings.Contains(text, "改成") || strings.Contains(text, "换成") || strings.Contains(text, "更换")
	fields[name] = agentorder.PatchValue{Value: value, Source: agentorder.SourceUser, ExplicitReplacement: explicit, Evidence: evidence}
}

func candidateFromSnapshot(snapshot agentorder.DraftSnapshot) []workflows.IntentCandidate {
	c := workflows.IntentCandidate{}
	for name, field := range snapshot.Fields {
		switch name {
		case "product_sku":
			c.ProductSKU, _ = field.Value.(string)
		case "quantity":
			c.Quantity = asInt64(field.Value)
		case "receive_address":
			c.ReceiveAddress, _ = field.Value.(string)
		case "customer_id":
			c.CustomerID, _ = field.Value.(string)
		case "payment_term":
			c.PaymentTerm, _ = field.Value.(string)
		case "warehouse_id":
			c.WarehouseID, _ = field.Value.(string)
		}
	}
	if c.ProductSKU == "" && c.Quantity == 0 && c.ReceiveAddress == "" && c.CustomerID == "" {
		return nil
	}
	return []workflows.IntentCandidate{c}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
