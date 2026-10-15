package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/repo"
)

// ── 回调 → HTTP 动作转接（07 §4.2；W5 前置单测）──

// fakeActions 记录 ActionClient 调用。
type fakeActions struct {
	orders   []string // tenant:order:signal
	preoders []string // tenant:preOrderID:signal
	failNext bool
}

func (f *fakeActions) OrderSignal(ctx context.Context, tenantID, orderID, signal, userID string, payload map[string]any) error {
	if f.failNext {
		f.failNext = false
		return errors.New("api error")
	}
	f.orders = append(f.orders, tenantID+":"+orderID+":"+signal)
	return nil
}

func (f *fakeActions) PreOrderSignal(ctx context.Context, tenantID, preOrderID, signal, userID string, payload map[string]any) error {
	if f.failNext {
		f.failNext = false
		return errors.New("api error")
	}
	f.preoders = append(f.preoders, tenantID+":"+preOrderID+":"+signal)
	return nil
}

// fakeTasks 内存 TaskStore。
type fakeTasks struct {
	byID map[string]*repo.ApprovalTask
}

func (f *fakeTasks) GetApprovalTask(ctx context.Context, tenantID, taskID string) (*repo.ApprovalTask, error) {
	t, ok := f.byID[taskID]
	if !ok {
		return nil, errors.New("task not found")
	}
	return t, nil
}

func testTask(id, orderID, stage, kind string) *repo.ApprovalTask {
	return &repo.ApprovalTask{ID: id, TenantID: "chang-an", OrderID: orderID, StageCode: stage, Kind: kind}
}

func TestParseEventKey(t *testing.T) {
	cases := []struct {
		key     string
		prefix  string
		arg     string
		wantErr bool
	}{
		{"approve:AT-1", "approve", "AT-1", false},
		{"reject:AT-9", "reject", "AT-9", false},
		{"preorder:confirm:PRE-1", "preorder:confirm", "PRE-1", false},
		{"preorder:ignore:PRE-2", "preorder:ignore", "PRE-2", false},
		{"preorder:modify:PRE-3", "preorder:modify", "PRE-3", false},
		{"approve", "", "", true},
		{"preorder:PRE-1", "", "", true},
		{"preorder:confirm:", "", "", true},
		{"bogus:x", "", "", true},
		{"", "", "", true},
	}
	for _, c := range cases {
		p, a, err := parseEventKey(c.key)
		if c.wantErr {
			require.Error(t, err, c.key)
			continue
		}
		require.NoError(t, err, c.key)
		require.Equal(t, c.prefix, p)
		require.Equal(t, c.arg, a)
	}
}

func TestCallbackHandlerApproveReject(t *testing.T) {
	actions := &fakeActions{}
	tasks := &fakeTasks{byID: map[string]*repo.ApprovalTask{
		"AT-1": testTask("AT-1", "ORD-1", "ORD", "PRICE"),
		"AT-2": testTask("AT-2", "ORD-1", "INV", "INVOICE_FINANCE_ISSUE"),
	}}
	h := &CallbackHandler{Actions: actions, Tasks: tasks}

	// 审价闸门通过
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:AT-1", FromUserID: "u-supervisor",
	}))
	require.Equal(t, []string{"chang-an:ORD-1:price-review"}, actions.orders)

	// 驳回 → verdict REJECTED（终态/驳回链）
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "reject:AT-1", FromUserID: "u-supervisor",
	}))
	require.Len(t, actions.orders, 2)

	// 开票闸门映射
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "approve:AT-2", FromUserID: "u-accounting",
	}))
	require.Equal(t, "chang-an:ORD-1:invoice-issued", actions.orders[2])

	// 任务不存在 → 错误
	err := h.Handle(context.Background(), CallbackPayload{TenantID: "chang-an", EventKey: "approve:AT-404"})
	require.Error(t, err)

	// 未知 EventKey → 拒绝
	err = h.Handle(context.Background(), CallbackPayload{TenantID: "chang-an", EventKey: "bogus:x"})
	require.ErrorIs(t, err, ErrUnknownEventKey)
}

func TestCallbackHandlerPreOrder(t *testing.T) {
	actions := &fakeActions{}
	h := &CallbackHandler{Actions: actions}

	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:confirm:PRE-1", FromUserID: "u-sales",
	}))
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:ignore:PRE-2", FromUserID: "u-sales",
	}))
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:modify:PRE-3", FromUserID: "u-sales",
	}))
	require.Equal(t, []string{"chang-an:PRE-1:preorder-confirm", "chang-an:PRE-2:preorder-ignore", "chang-an:PRE-3:preorder-confirm"}, actions.preoders)

	// 动作失败向上传播（上层告警/重试）
	actions.failNext = true
	require.Error(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:confirm:PRE-9", FromUserID: "u-sales",
	}))
}

func TestCallbackHandlerStageNoMapping(t *testing.T) {
	actions := &fakeActions{}
	tasks := &fakeTasks{byID: map[string]*repo.ApprovalTask{
		"AT-X": testTask("AT-X", "ORD-9", "ORD", "BOGUS_KIND"),
	}}
	h := &CallbackHandler{Actions: actions, Tasks: tasks}
	err := h.Handle(context.Background(), CallbackPayload{TenantID: "chang-an", EventKey: "approve:AT-X"})
	require.ErrorIs(t, err, ErrTaskStageNoSignal)
	require.Empty(t, actions.orders) // 不投递任何动作
}

// TestHTTPActionClient 真实 HTTP 路径：动作 payload/头/状态码语义。
func TestHTTPActionClient(t *testing.T) {
	var gotMethod, gotPath, gotUser string
	var gotPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotUser = r.Header.Get("X-User-Id")
		require.NoError(t, jsonDecode(r, &gotPayload))
		if strings.HasSuffix(r.URL.Path, "signal/price-review") {
			// 幂等回执：已推进 → 409（转接层视为错误向上传播）
			http.Error(w, `{"error":"already decided"}`, http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	c := &HTTPActionClient{BaseURL: ts.URL, UserID: "u-supervisor", HTTP: ts.Client()}
	require.NoError(t, c.OrderSignal(context.Background(), "chang-an", "ORD-1", "ship-action", "u-wh-yangyuan",
		map[string]any{"verdict": "APPROVED"}))
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/api/v1/orders/ORD-1/signal/ship-action", gotPath)
	require.Equal(t, "u-wh-yangyuan", gotUser) // 0.1：以回调触发人身份投递
	require.Equal(t, "APPROVED", gotPayload["verdict"])

	// 非 2xx（409 幂等回执）→ 错误传播
	err := c.OrderSignal(context.Background(), "chang-an", "ORD-1", "price-review", "u-supervisor",
		map[string]any{"verdict": "APPROVED"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "409")

	require.NoError(t, c.PreOrderSignal(context.Background(), "chang-an", "PRE-1", "preorder-confirm", "u-sales",
		map[string]any{"confirmed": true, "operator": "u-sales"}))
	require.Equal(t, "/api/v1/preorders/PRE-1/signal/preorder-confirm", gotPath)
}

func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// TestHTTPActionClientErrorBranches 防御分支：网络错误 / 响应解码失败 / nil Logf。
func TestHTTPActionClientErrorBranches(t *testing.T) {
	// 连接错误（不可达地址）→ error 传播
	c := &HTTPActionClient{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}
	err := c.OrderSignal(context.Background(), "chang-an", "ORD-1", "price-review", "u-supervisor", map[string]any{"verdict": "APPROVED"})
	require.Error(t, err)

	// 非 JSON 错误体 → 回退 resp.Status
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer ts.Close()
	c2 := &HTTPActionClient{BaseURL: ts.URL, UserID: "u-supervisor", HTTP: ts.Client()}
	err = c2.PreOrderSignal(context.Background(), "chang-an", "PRE-1", "preorder-confirm", "u-sales", map[string]any{"confirmed": true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "502")

	// 无 Logf 的 Handler 正常路由（nil 检查分支）
	actions := &fakeActions{}
	h := &CallbackHandler{Actions: actions}
	require.NoError(t, h.Handle(context.Background(), CallbackPayload{
		TenantID: "chang-an", EventKey: "preorder:ignore:PRE-9", FromUserID: "u-sales",
	}))
	require.Equal(t, []string{"chang-an:PRE-9:preorder-ignore"}, actions.preoders)
}
