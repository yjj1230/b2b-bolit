package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.temporal.io/sdk/testsuite"

	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/workflows"
)

// ── LocalRunner：本地内嵌工作流运行器（HTTP API + 企微 mock 测试模式）──
//
// 目的（用户要求）：官方企微 API 与第三方 API 的选型推迟到最后时刻；在此之前，
// 测试走 HTTP API + 企微 mock，不依赖 Temporal 服务。LocalRunner 用 Temporal
// TestSuite 环境在进程内执行 OrderWorkflow/PreOrderWorkflow，信号经 HTTP 投递。
// 生产模式（cmd/api 默认）使用 Temporal 客户端（workflows.TemporalStarter）。
//
// 注意：每个工作流一个 test env（内存态）；重启即丢失，仅用于功能/演示测试。

type localRun struct {
	env interface {
		SignalWorkflow(name string, arg any)
	}
	done    chan struct{}
	started chan struct{} // env 主循环就绪（信号可安全投递）
}

// LocalRunner 本地运行器。
type LocalRunner struct {
	mu        sync.Mutex
	runs      map[string]*localRun // workflowID → run
	store     workflows.Store
	data      orderdata.Provider
	notifier  workflows.Notifier
	checker   workflows.AuthChecker
	policy    integration.BusinessConfigProvider // 08 §5.2：Timer 时长从接入层策略读取
	assignees workflows.AssigneeConfig           // ROL-01：审批人路由（默认 DefaultAssignees）
	logf      func(string, ...any)
}

// NewLocalRunner 构造本地运行器（接入层 Static 基线：24h/48h/5d，与历史硬编码一致）。
func NewLocalRunner(store workflows.Store, notifier workflows.Notifier, checker workflows.AuthChecker) *LocalRunner {
	return NewLocalRunnerWithPolicy(store, notifier, checker, integration.NewStaticConfigProvider())
}

// NewLocalRunnerWithPolicy 构造并注入业务配置 Provider（08 §5.2 接入层契约；
// 测试可置 0 关闭 Timer 闸门，获得确定性信号驱动）。Data 为空时保留旧测试兼容路径。
func NewLocalRunnerWithPolicy(store workflows.Store, notifier workflows.Notifier, checker workflows.AuthChecker, policy integration.BusinessConfigProvider) *LocalRunner {
	return NewLocalRunnerWithPolicyAndData(store, nil, notifier, checker, policy)
}

// NewLocalRunnerWithPolicyAndData 同 NewLocalRunnerWithPolicy，并注入订单数据实体 Provider。
func NewLocalRunnerWithPolicyAndData(store workflows.Store, data orderdata.Provider, notifier workflows.Notifier, checker workflows.AuthChecker, policy integration.BusinessConfigProvider) *LocalRunner {
	return &LocalRunner{
		runs: map[string]*localRun{}, store: store, data: data, notifier: notifier, checker: checker,
		policy: policy, assignees: workflows.DefaultAssignees(),
		logf: func(string, ...any) {},
	}
}

// SetAssignees 注入审批人路由（ROL-01：B2B_ASSIGNEE_* 配置化）。
func (l *LocalRunner) SetAssignees(a workflows.AssigneeConfig) { l.assignees = a }

// activities 按是否配置订单数据实体 Provider 构造 Activity 集。
func (l *LocalRunner) activities() *workflows.Activities {
	if l.data != nil {
		return workflows.NewActivitiesWithData(l.store, l.notifier, l.checker, l.data)
	}
	return workflows.NewActivities(l.store, l.notifier, l.checker)
}

// SetLogger 注入日志。
func (l *LocalRunner) SetLogger(f func(string, ...any)) { l.logf = f }

// StartOrderWorkflow 启动订单主流程（本地 env）。
func (l *LocalRunner) StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	ord, err := l.store.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return "", err
	}
	workflowID := WorkflowID(tenantID, ord.IdempotencyKey)
	l.mu.Lock()
	if _, ok := l.runs[workflowID]; ok {
		l.mu.Unlock()
		return workflowID, nil // 幂等启动
	}
	l.mu.Unlock()

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflows.OrderWorkflow)
	env.RegisterActivity(l.activities())
	// 就绪握手：0 延迟回调在 env 主循环启动后立即触发（消除 ExecuteWorkflow 启动竞态）
	started := make(chan struct{})
	env.RegisterDelayedCallback(func() { close(started) }, 0)

	params := workflows.OrderWorkflowParams{
		TenantID: tenantID, OrderID: orderID,
		ApprovalRemindAfter:   l.policy.RemindAfter(),           // 接入层业务配置（Static 基线 = 24h）
		ApprovalEscalateAfter: l.policy.EscalateAfter(),         // 48h
		ReceiptExceptionAfter: l.policy.ReceiptExceptionAfter(), // 5d
		Assignees:             l.assignees,
	}
	done := make(chan struct{})
	go func() {
		env.ExecuteWorkflow(workflows.OrderWorkflow, params)
		if err := env.GetWorkflowError(); err != nil {
			l.logf("local order workflow %s ended with error: %v", workflowID, err)
		}
		close(done)
		l.mu.Lock()
		delete(l.runs, workflowID)
		l.mu.Unlock()
	}()
	l.mu.Lock()
	l.runs[workflowID] = &localRun{env: env, done: done, started: started}
	l.mu.Unlock()
	return workflowID, nil
}

// StartPreOrderWorkflow 启动预订单工作流（本地 env；senderID 为 07 §3.2 权限校验输入）。
func (l *LocalRunner) StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	workflowID := "preorder-" + tenantID + "-" + preOrderID
	l.mu.Lock()
	if _, ok := l.runs[workflowID]; ok {
		l.mu.Unlock()
		return workflowID, nil
	}
	l.mu.Unlock()

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflows.PreOrderWorkflow)
	env.RegisterActivity(l.activities())
	started := make(chan struct{})
	env.RegisterDelayedCallback(func() { close(started) }, 0)
	done := make(chan struct{})
	go func() {
		env.ExecuteWorkflow(workflows.PreOrderWorkflow, workflows.PreOrderWorkflowParams{
			TenantID: tenantID, PreOrderID: preOrderID, SenderID: senderID,
			EscalateAfter: l.policy.PreOrderEscalateAfter(), // 接入层业务配置（Static 基线 = 24h）
		})
		if err := env.GetWorkflowError(); err != nil {
			l.logf("local preorder workflow %s ended with error: %v", workflowID, err)
		}
		close(done)
		l.mu.Lock()
		delete(l.runs, workflowID)
		l.mu.Unlock()
	}()
	l.mu.Lock()
	l.runs[workflowID] = &localRun{env: env, done: done, started: started}
	l.mu.Unlock()
	return workflowID, nil
}

// SignalWorkflow 投递信号到本地 env（等待 env 就绪，消除启动竞态）。
func (l *LocalRunner) SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error {
	l.mu.Lock()
	run, ok := l.runs[workflowID]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("workflow %s not running (local runner)", workflowID)
	}
	select {
	case <-run.started:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("workflow %s env not ready (local runner)", workflowID)
	}
	run.env.SignalWorkflow(signalName, payload)
	return nil
}

// Wait 等待工作流结束（测试/演示用）。
func (l *LocalRunner) Wait(ctx context.Context, workflowID string) error {
	l.mu.Lock()
	run, ok := l.runs[workflowID]
	l.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ Starter = (*LocalRunner)(nil)

// WaitAll 等待所有已启动的本地工作流完成（最多 timeout；测试环境隔离用，
// 避免失败用例遗留的工作流在下一用例 schema 重置后继续活动）。
func (l *LocalRunner) WaitAll(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		l.mu.Lock()
		runs := make([]<-chan struct{}, 0, len(l.runs))
		for id, r := range l.runs {
			select {
			case <-r.done:
				delete(l.runs, id) // 已结束：移出，避免空转
			default:
				runs = append(runs, r.done)
			}
		}
		l.mu.Unlock()
		if len(runs) == 0 {
			return
		}
		for _, done := range runs {
			select {
			case <-done:
			case <-time.After(time.Until(deadline)):
				if time.Now().After(deadline) {
					l.logf("local runner: %d workflow(s) still running after %v (leaked)", len(runs), timeout)
					return
				}
			}
		}
	}
}

// fakeStarter 测试替身（httptest 场景）。
type fakeStarter struct {
	mu      sync.Mutex
	store   workflows.Store
	started []string
	signals []fakeSignal
}

type fakeSignal struct {
	WorkflowID string
	Name       string
	Payload    any
}

func newFakeStarter(store workflows.Store) *fakeStarter { return &fakeStarter{store: store} }

func (f *fakeStarter) StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	ord, err := f.store.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := WorkflowID(tenantID, ord.IdempotencyKey)
	f.started = append(f.started, id)
	return id, nil
}

func (f *fakeStarter) StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "preorder-" + tenantID + "-" + preOrderID
	f.started = append(f.started, id)
	return id, nil
}

func (f *fakeStarter) SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, fakeSignal{WorkflowID: workflowID, Name: signalName, Payload: payload})
	return nil
}
