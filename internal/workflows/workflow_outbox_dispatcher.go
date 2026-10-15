package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"b2b_pilot_v3/internal/repo"
)

type workflowOutboxStore interface {
	ClaimWorkflowOutbox(context.Context, int, time.Duration) ([]repo.WorkflowOutboxEvent, error)
	MarkWorkflowOutboxDone(context.Context, int64) error
	RetryWorkflowOutbox(context.Context, int64, time.Time, string) error
	DeadLetterWorkflowOutbox(context.Context, repo.WorkflowOutboxEvent, string) error
}

type workflowStartPayload struct {
	TenantID   string `json:"tenant_id"`
	OrderID    string `json:"order_id"`
	WorkflowID string `json:"workflow_id"`
}

// WorkflowOutboxDispatcher 将数据库事务提交的启动事件可靠投递给 Temporal。
type WorkflowOutboxDispatcher struct {
	Store        workflowOutboxStore
	Client       client.Client
	TaskQueue    string
	PollInterval time.Duration
	Lease        time.Duration
	MaxAttempts  int
}

func (d *WorkflowOutboxDispatcher) DispatchOnce(ctx context.Context) error {
	if d.Store == nil || d.Client == nil {
		return fmt.Errorf("workflow outbox dispatcher not configured")
	}
	lease := d.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	events, err := d.Store.ClaimWorkflowOutbox(ctx, 50, lease)
	if err != nil {
		return err
	}
	for _, event := range events {
		var p workflowStartPayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			_ = d.Store.DeadLetterWorkflowOutbox(ctx, event, err.Error())
			continue
		}
		if p.WorkflowID == "" {
			p.WorkflowID = event.WorkflowID
		}
		if p.OrderID == "" || p.WorkflowID == "" {
			_ = d.Store.DeadLetterWorkflowOutbox(ctx, event, "workflow outbox payload missing order_id or workflow_id")
			continue
		}
		tenantID := p.TenantID
		if tenantID == "" {
			tenantID = event.TenantID
		}
		_, err := d.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: p.WorkflowID, TaskQueue: d.TaskQueue}, OrderWorkflow,
			OrderWorkflowParams{TenantID: tenantID, OrderID: p.OrderID})
		if err == nil || isWorkflowAlreadyStarted(err) {
			if markErr := d.Store.MarkWorkflowOutboxDone(ctx, event.ID); markErr != nil {
				return markErr
			}
			continue
		}
		max := d.MaxAttempts
		if max <= 0 {
			max = 8
		}
		if event.Attempts >= max {
			if dlErr := d.Store.DeadLetterWorkflowOutbox(ctx, event, err.Error()); dlErr != nil {
				return dlErr
			}
			continue
		}
		backoff := time.Duration(1<<min(event.Attempts, 6)) * time.Second
		if retryErr := d.Store.RetryWorkflowOutbox(ctx, event.ID, time.Now().UTC().Add(backoff), err.Error()); retryErr != nil {
			return retryErr
		}
	}
	return nil
}

func (d *WorkflowOutboxDispatcher) Run(ctx context.Context) error {
	interval := d.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := d.DispatchOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// 数据库或 Temporal 短暂故障由下一轮重试，不能退出 worker。
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isWorkflowAlreadyStarted(err error) bool {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &already)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
