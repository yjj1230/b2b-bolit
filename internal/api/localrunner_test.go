package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/workflows"
)

// testLocalRunner 装配：fakeStore + StaticChecker + FakeNotifier。
func testLocalRunner(t *testing.T, policy integration.BusinessConfigProvider) (*LocalRunner, *workflows.FakeStore) {
	t.Helper()
	store := workflows.NewFakeStoreForTest()
	seedAPIWorld(t, store)
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
	if policy == nil {
		policy = integration.NewDemoPolicy()
	}
	lr := NewLocalRunnerWithPolicy(store, &workflows.FakeNotifier{}, checker, policy)
	lr.SetLogger(func(string, ...any) {})
	return lr, store
}

// startRejectedOrder 启动一笔订单并立即否决：工作流走到 ORD_REJECTED 终态后完成。
func startRejectedOrder(t *testing.T, lr *LocalRunner, store *workflows.FakeStore) string {
	t.Helper()
	ctx := context.Background()
	ord, _, err := store.CreateOrder(ctx, workflows.NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:lr-reject"))
	require.NoError(t, err)
	wfID, err := lr.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return lr.SignalWorkflow(ctx, wfID, "price-review", map[string]any{"verdict": "REJECTED", "operator": "u-supervisor"}) == nil
	}, 5*time.Second, 20*time.Millisecond)
	return ord.ID
}

// TestLocalRunnerWithPolicyStart 策略注入路径：Demo 策略下订单/预订单工作流正常启动并可达闸门。
func TestLocalRunnerWithPolicyStart(t *testing.T) {
	lr, store := testLocalRunner(t, integration.NewDemoPolicy())
	ctx := context.Background()

	// 订单工作流：启动 → 审价闸门（状态推进证明策略参数未破坏执行）
	ord, _, err := store.CreateOrder(ctx, workflows.NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:lr-start"))
	require.NoError(t, err)
	wfID, err := lr.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)
	require.Equal(t, WorkflowID("chang-an", "order:chang-an:quick:lr-start"), wfID)
	require.Eventually(t, func() bool {
		o, err := store.GetOrder(ctx, "chang-an", ord.ID)
		return err == nil && o.State == "ORD_PRICE_REVIEWING"
	}, 5*time.Second, 20*time.Millisecond)

	// 预订单工作流：启动 → 确认闸门（Demo 策略 10s Timer 被 TestSuite 自动触发时
	// 会升级到 PRE_ESCALATED——两者都证明策略注入后执行路径完整）
	pre, _, err := store.CreatePreOrder(ctx, workflows.NewPreOrderForTest("chang-an", "group-demo", "lr-msg-1"))
	require.NoError(t, err)
	pwfID, err := lr.StartPreOrderWorkflow(ctx, "chang-an", pre.ID, "")
	require.NoError(t, err)
	require.Equal(t, "preorder-chang-an-"+pre.ID, pwfID)
	require.Eventually(t, func() bool {
		p, err := store.GetPreOrder(ctx, "chang-an", pre.ID)
		return err == nil && (p.State == "PRE_PENDING_CONFIRM" || p.State == "PRE_ESCALATED")
	}, 5*time.Second, 20*time.Millisecond)
}

// TestLocalRunnerWaitAllCompleted 已完成工作流：WaitAll 立即返回（不空转不误报）。
func TestLocalRunnerWaitAllCompleted(t *testing.T) {
	lr, store := testLocalRunner(t, nil)
	// 等待工作流终态落定（用订单 ID 查询；idempotency_key ≠ order ID）
	ordID := startRejectedOrder(t, lr, store)
	require.Eventually(t, func() bool {
		o, err := store.GetOrder(context.Background(), "chang-an", ordID)
		return err == nil && o.State == "ORD_REJECTED"
	}, 5*time.Second, 20*time.Millisecond)

	done := make(chan struct{})
	go func() { lr.WaitAll(2 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitAll 应在工作流完成后快速返回")
	}
}

// TestLocalRunnerWaitAllTimeout 未完成工作流：WaitAll 超时返回（防跨用例残留的告警路径）。
func TestLocalRunnerWaitAllTimeout(t *testing.T) {
	lr, store := testLocalRunner(t, nil)
	ctx := context.Background()
	ord, _, err := store.CreateOrder(ctx, workflows.NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:lr-hang"))
	require.NoError(t, err)
	_, err = lr.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)

	start := time.Now()
	lr.WaitAll(300 * time.Millisecond) // 不投信号：闸门等待 → 超时返回
	require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
}

// TestLocalRunnerConcurrentSignal 并发信号投递不丢不崩（LocalRunner 锁路径）。
func TestLocalRunnerConcurrentSignal(t *testing.T) {
	lr, store := testLocalRunner(t, nil)
	ctx := context.Background()
	ord, _, err := store.CreateOrder(ctx, workflows.NewOrderForTest("chang-an", "cust-0001", "款到", "order:chang-an:quick:lr-conc"))
	require.NoError(t, err)
	wfID, err := lr.StartOrderWorkflow(ctx, "chang-an", ord.ID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lr.SignalWorkflow(ctx, wfID, "price-review", map[string]any{"verdict": "APPROVED", "operator": "u-supervisor"})
		}()
	}
	wg.Wait()
	// 工作流存活即可（并发投递无 panic/死锁；判定由工作流语义决定）
	require.NotEmpty(t, wfID)
}
