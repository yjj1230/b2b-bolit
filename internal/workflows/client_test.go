package workflows

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// ── TemporalStarter（client.go；fake client 覆盖启动/幂等/信号）──

type fakeTemporalClient struct {
	client.Client // 内嵌满足接口；仅覆盖被测方法
	startErr      error
	started       []string
	lastOpts      *client.StartWorkflowOptions
	signaled      []string
}

func (f *fakeTemporalClient) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started = append(f.started, options.ID)
	cp := options
	f.lastOpts = &cp
	return nil, nil
}

func (f *fakeTemporalClient) SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error {
	f.signaled = append(f.signaled, workflowID+":"+signalName)
	return nil
}

type fakeWorkflowRun struct{ id string }

func (f *fakeWorkflowRun) GetID() string    { return f.id }
func (f *fakeWorkflowRun) GetRunID() string { return "run-1" }

func TestTemporalStarter(t *testing.T) {
	store := newFakeStore()
	store.SeedCustomer("chang-an", "cust-0001", "A")
	fc := &fakeTemporalClient{}
	ts := NewTemporalStarter(fc, store)
	ctx := context.Background()

	// 启动订单工作流（WorkflowID = order-tenant-idemKey）
	ord, _, err := store.CreateOrder(ctx, NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:t1"))
	require.NoError(t, err)
	wid, err := ts.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)
	require.Equal(t, "order-chang-an-order:chang-an:quick:t1", wid)
	require.Equal(t, []string{wid}, fc.started)

	// 幂等启动：AlreadyStarted → 返回同 ID（不重复启动）
	fc.startErr = &serviceerror.WorkflowExecutionAlreadyStarted{}
	wid2, err := ts.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)
	require.Equal(t, wid, wid2)
	require.Len(t, fc.started, 1) // 未新增启动

	// 其他错误 → 报错
	fc.startErr = &serviceerror.InvalidArgument{Message: "boom"}
	_, err = ts.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.Error(t, err)

	// 未知订单 → 错误
	_, err = ts.StartOrderWorkflow(ctx, "chang-an", "ORD-nope")
	require.Error(t, err)

	// 预订单启动（幂等 + 错误）
	po, _, err := store.CreatePreOrder(ctx, NewPreOrderForTest("chang-an", "g1", "m1"))
	require.NoError(t, err)
	fc.startErr = nil
	wid3, err := ts.StartPreOrderWorkflow(ctx, "chang-an", po.ID, "u-sales")
	require.NoError(t, err)
	require.Equal(t, "preorder-chang-an-"+po.ID, wid3)
	fc.startErr = &serviceerror.WorkflowExecutionAlreadyStarted{}
	wid4, err := ts.StartPreOrderWorkflow(ctx, "chang-an", po.ID, "u-sales")
	require.NoError(t, err)
	require.Equal(t, wid3, wid4)
	fc.startErr = &serviceerror.InvalidArgument{Message: "x"}
	_, err = ts.StartPreOrderWorkflow(ctx, "chang-an", po.ID, "u-sales")
	require.Error(t, err)

	// 信号
	require.NoError(t, ts.SignalWorkflow(ctx, wid, SignalPriceReview, PriceReviewSignal{}))
	require.Equal(t, []string{wid + ":price-review"}, fc.signaled)
}

// 影子重放口径（A9）：shadow- 前缀 + 影子队列 + TerminateExisting / AllowDuplicate，
// 保证「修一次，全量重放一次」不会被上一次仍 RUNNING 的执行粘住。
func TestShadowStarterReplayPolicy(t *testing.T) {
	store := newFakeStore()
	store.SeedCustomer("chang-an-shadow", "cust-0001", "A")
	fc := &fakeTemporalClient{}
	ts := NewTemporalStarter(fc, store).ConfigureQueue("b2b-shadow-order", ShadowWorkflowIDPrefix)
	ctx := context.Background()

	ord, _, err := store.CreateOrder(ctx, NewOrderForTest("chang-an-shadow", "cust-0001", "款到", "order:chang-an-shadow:quick:shadow-flow-1"))
	require.NoError(t, err)
	wid, err := ts.StartShadowOrderWorkflow(ctx, "chang-an-shadow", ord.ID)
	require.NoError(t, err)
	require.Equal(t, "shadow-order-chang-an-shadow-order:chang-an-shadow:quick:shadow-flow-1", wid)
	require.NotNil(t, fc.lastOpts)
	require.Equal(t, "b2b-shadow-order", fc.lastOpts.TaskQueue)
	require.Equal(t, enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE, fc.lastOpts.WorkflowIDReusePolicy)
	require.Equal(t, enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING, fc.lastOpts.WorkflowIDConflictPolicy)

	po, _, err := store.CreatePreOrder(ctx, NewPreOrderForTest("chang-an-shadow", "g-shadow", "m1"))
	require.NoError(t, err)
	wid2, err := ts.StartShadowPreOrderWorkflow(ctx, "chang-an-shadow", po.ID, "u-shadow-sales")
	require.NoError(t, err)
	require.Equal(t, "shadow-preorder-chang-an-shadow-"+po.ID, wid2)
	require.Equal(t, enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING, fc.lastOpts.WorkflowIDConflictPolicy)
}

// Get 满足 WorkflowRun 接口。
func (f *fakeWorkflowRun) Get(ctx context.Context, valuePtr interface{}) error { return nil }

// GetWithOptions 满足 WorkflowRun 接口。
func (f *fakeWorkflowRun) GetWithOptions(ctx context.Context, options client.WorkflowRunGetOptions, valuePtr interface{}) error {
	return nil
}
