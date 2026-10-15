package workflows

import (
	"context"
	"errors"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"b2b_pilot_v3/internal/integration"
)

// ── TemporalStarter：生产模式工作流启动/信号（cmd/api 默认；02 §5 层 2 幂等启动）──

// TaskQueue A 轨默认队列（b2b-order）。
// 影子轨必须经 B2B_TASK_QUEUE=b2b-shadow-order 注入（12 §7 A-03；shadowguard 强制）。
const TaskQueue = "b2b-order"

// ShadowWorkflowIDPrefix 影子轨 WorkflowID 前缀（12 §7：与 A 轨 ID 空间完全隔离）。
const ShadowWorkflowIDPrefix = "shadow-"

// TemporalStarter 基于 Temporal 客户端的 Starter 实现。
type TemporalStarter struct {
	client    client.Client
	store     Store
	policy    integration.BusinessConfigProvider // 业务配置（接入层；默认 Static 基线值）
	assignees AssigneeConfig                     // 审批人路由（默认 DefaultAssignees）
	// taskQueue / workflowIDPrefix A-03 Temporal 隔离：worker 与 Starter 同队列注入，
	// 影子轨 WorkflowID 加 shadow- 前缀（SH-14）。
	taskQueue        string
	workflowIDPrefix string
}

// NewTemporalStarter 构造（使用接入层 Static 基线配置：24h/48h/5d，行为不变）。
func NewTemporalStarter(c client.Client, store Store) *TemporalStarter {
	return NewTemporalStarterWithPolicy(c, store, integration.NewStaticConfigProvider())
}

// NewTemporalStarterWithPolicy 构造并注入业务配置 Provider（接入层契约，测试/多配置源可替换）。
func NewTemporalStarterWithPolicy(c client.Client, store Store, policy integration.BusinessConfigProvider) *TemporalStarter {
	return &TemporalStarter{
		client: c, store: store, policy: policy, assignees: DefaultAssignees(),
		taskQueue: TaskQueue,
	}
}

// SetAssignees 注入审批人路由（ROL-01：B2B_ASSIGNEE_* 配置化）。
func (t *TemporalStarter) SetAssignees(a AssigneeConfig) { t.assignees = a }

// ConfigureQueue 注入 task queue 与 WorkflowID 前缀（SH-14 / A-03）。
// 影子轨：ConfigureQueue("b2b-shadow-order", "shadow-")；A 轨零调用即默认 b2b-order。
func (t *TemporalStarter) ConfigureQueue(taskQueue, workflowIDPrefix string) *TemporalStarter {
	t.taskQueue = taskQueue
	t.workflowIDPrefix = workflowIDPrefix
	return t
}

func (t *TemporalStarter) queue() string {
	if t.taskQueue == "" {
		return TaskQueue
	}
	return t.taskQueue
}

// StartOrderWorkflow 幂等启动 OrderWorkflow（WorkflowID = <prefix>order-<tenant>-<idemKey>，
// 重复 StartWorkflow → 回查返回同一流程，02 §5 层 2）。
func (t *TemporalStarter) StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	params := OrderWorkflowParams{
		TenantID: tenantID, OrderID: orderID,
		// Timer 时长来自接入层业务配置（Static 基线 = 24h/48h/5d，行为不变）
		ApprovalRemindAfter:   t.policy.RemindAfter(),
		ApprovalEscalateAfter: t.policy.EscalateAfter(),
		ReceiptExceptionAfter: t.policy.ReceiptExceptionAfter(),
		Assignees:             t.assignees,
	}
	return t.startOrderWorkflow(ctx, tenantID, orderID, params)
}

// StartShadowOrderWorkflow 影子轨建单（12 §5 步骤 2）：
// Shadow=true → Timer 全部不调度、推进只认信号；时长参数清零可确定性重放（S-06）。
func (t *TemporalStarter) StartShadowOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	return t.startOrderWorkflow(ctx, tenantID, orderID, OrderWorkflowParams{
		TenantID: tenantID, OrderID: orderID, Shadow: true, Assignees: t.assignees,
	})
}

func (t *TemporalStarter) startOrderWorkflow(ctx context.Context, tenantID, orderID string, params OrderWorkflowParams) (string, error) {
	ord, err := t.store.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return "", err
	}
	workflowID := t.workflowIDPrefix + "order-" + tenantID + "-" + ord.IdempotencyKey
	opts := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: t.queue(),
	}
	if params.Shadow {
		// 影子可重建重放（S-铁5）：同一 WorkflowID 重放时，若上一次执行仍 RUNNING
		//（影子模式无 Timer，未走完的流程会一直等信号）则终止旧执行并开新 run；
		// 已结束执行按 AllowDuplicate 开新 run——保证「修一次，全量重放一次」可确定执行。
		opts.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE
		opts.WorkflowIDConflictPolicy = enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING
	}
	if _, err := t.client.ExecuteWorkflow(ctx, opts, OrderWorkflow, params); err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return workflowID, nil // 幂等启动：已有同 ID 流程
		}
		return "", fmt.Errorf("start order workflow: %w", err)
	}
	return workflowID, nil
}

// StartPreOrderWorkflow 幂等启动 PreOrderWorkflow。
func (t *TemporalStarter) StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	workflowID := t.workflowIDPrefix + "preorder-" + tenantID + "-" + preOrderID
	opts := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: t.queue(),
	}
	params := PreOrderWorkflowParams{
		TenantID: tenantID, PreOrderID: preOrderID, SenderID: senderID,
		EscalateAfter: t.policy.PreOrderEscalateAfter(), // 接入层业务配置（Static 基线 = 24h）
	}
	if _, err := t.client.ExecuteWorkflow(ctx, opts, PreOrderWorkflow, params); err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return workflowID, nil
		}
		return "", fmt.Errorf("start preorder workflow: %w", err)
	}
	return workflowID, nil
}

// StartShadowPreOrderWorkflow 影子轨预订单种子（PRE 段边覆盖用，12 §3.1 内部种子）。
func (t *TemporalStarter) StartShadowPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	workflowID := t.workflowIDPrefix + "preorder-" + tenantID + "-" + preOrderID
	opts := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: t.queue(),
		// 与影子订单同口径：可重建重放，RUNNING 旧执行终止后开新 run。
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}
	params := PreOrderWorkflowParams{TenantID: tenantID, PreOrderID: preOrderID, SenderID: senderID, Shadow: true}
	if _, err := t.client.ExecuteWorkflow(ctx, opts, PreOrderWorkflow, params); err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return workflowID, nil
		}
		return "", fmt.Errorf("start shadow preorder workflow: %w", err)
	}
	return workflowID, nil
}

// SignalWorkflow 投递信号。
func (t *TemporalStarter) SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error {
	return t.client.SignalWorkflow(ctx, workflowID, "", signalName, payload)
}
