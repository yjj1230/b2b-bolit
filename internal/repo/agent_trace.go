package repo

import (
	"context"
	"encoding/json"
)

type orderTracePayload struct {
	PreOrderID   string `json:"pre_order_id"`
	DraftVersion int64  `json:"draft_version"`
}

// GetAgentOrderTrace 返回订单与 Agent 草稿/outbox 的可审计关联。
func (r *Repo) GetAgentOrderTrace(ctx context.Context, tenantID, orderID string) (*AgentOrderTrace, error) {
	ord, err := r.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	trace := &AgentOrderTrace{TenantID: tenantID, OrderID: orderID, Confirmations: []PreOrderDecision{}, Outbox: []WorkflowOutboxEvent{}, DeadLetters: []DeadLetterEvent{}}
	var p orderTracePayload
	_ = json.Unmarshal(ord.Payload, &p)
	trace.PreOrderID, trace.DraftVersion = p.PreOrderID, p.DraftVersion
	if trace.PreOrderID != "" {
		po, err := r.GetPreOrder(ctx, tenantID, trace.PreOrderID)
		if err != nil {
			return nil, err
		}
		trace.GroupID, trace.MsgID = po.GroupID, po.MsgID
	}
	trace.Outbox, err = r.GetWorkflowOutboxByAggregate(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	for _, e := range trace.Outbox {
		if trace.WorkflowID == "" {
			trace.WorkflowID = e.WorkflowID
		}
	}
	return trace, nil
}
