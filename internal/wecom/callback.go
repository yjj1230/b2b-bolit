package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"b2b_pilot_v3/internal/repo"
)

// ── 企微回调 → HTTP API 动作转接（07 §4.2：EventKey 协议；企微 = API 之上的转接层）──

// ActionClient 转接动作执行契约：把企微按钮事件翻译为 HTTP API 调用。
// 实现必须走真实 HTTP（与 scripts/fullchain 同一调用面），业务语义全部在 API 层。
type ActionClient interface {
	// OrderSignal 投递订单审批信号（verdict 由 payload 携带；userID = 回调触发人）。
	OrderSignal(ctx context.Context, tenantID, orderID, signal, userID string, payload map[string]any) error
	// PreOrderSignal 投递预订单信号（confirm/ignore/modify；销售唯一确认角色校验在 API 层）。
	PreOrderSignal(ctx context.Context, tenantID, preOrderID, signal, userID string, payload map[string]any) error
}

// HTTPActionClient ActionClient 的 HTTP 实现（转接层默认）。
type HTTPActionClient struct {
	BaseURL string // cmd/api 服务地址（如 http://127.0.0.1:18080）
	UserID  string // 回调回执用户（企微 FromUserID → X-User-Id；权限由 API 层判定）
	HTTP    *http.Client
}

// doAs 以回调触发人身份投递（N-02：销售唯一确认角色校验在 API 层判定）。
func (c *HTTPActionClient) doAs(ctx context.Context, method, path, userID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal action payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", userID)
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("action %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 2xx 之外的响应（含 409 幂等回执）视为失败，由上层决定是否重试/告警
		var out struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.Error == "" {
			out.Error = resp.Status
		}
		return fmt.Errorf("action %s %s: status=%d (%s)", method, path, resp.StatusCode, out.Error)
	}
	return nil
}

func (c *HTTPActionClient) OrderSignal(ctx context.Context, tenantID, orderID, signal, userID string, payload map[string]any) error {
	return c.doAs(ctx, http.MethodPost, fmt.Sprintf("/api/v1/orders/%s/signal/%s", orderID, signal), userID, payload)
}

func (c *HTTPActionClient) PreOrderSignal(ctx context.Context, tenantID, preOrderID, signal, userID string, payload map[string]any) error {
	return c.doAs(ctx, http.MethodPost, fmt.Sprintf("/api/v1/preorders/%s/signal/%s", preOrderID, signal), userID, payload)
}

// TaskStore 审批任务定位（approve/reject EventKey 需要 taskID → 订单/闸门）。
// repo.Repo 满足该契约（GetApprovalTask）。
type TaskStore interface {
	GetApprovalTask(ctx context.Context, tenantID, taskID string) (*repo.ApprovalTask, error)
}

// stageAction 审批任务（StageCode:Kind）→ 订单信号与按钮 verdict 翻译
// （07 §4.2；卡片「通过/驳回」的 verdict 按信号语义映射，与 internal/workflows
// order.go 信号结构注释一致）。SHP 发货需数量参数，不走「是/否」卡片——
// 经 S1 侧边栏/H5 表单操作。
type stageAction struct {
	signal    string
	okVerdict string // 卡片「通过」
	noVerdict string // 卡片「驳回」
}

var stageSignal = map[string]stageAction{
	"ORD:PRICE":    {"price-review", "APPROVED", "REJECTED"},
	"ORD:CHAIRMAN": {"chairman-review", "APPROVED", "REJECTED"},
	// 注意：STK:STOCK_REVIEW（商务备货）不在卡片映射内——备货通过必须携带选仓
	// （warehouse_id），走 H5 表单信号（08-api §3 stock-review），卡片 approve 无法携带选仓。
	"STK:STOCK_WAREHOUSE":       {"warehouse-confirm", "APPROVED", "RETURNED"},
	"STK:PAYMENT":               {"payment-confirm", "APPROVED", "RETURNED"},
	"INV:RECEIPT_EX":            {"receipt-ex-review", "APPROVED", "REJECTED"},
	"INV:INVOICE_FINANCE_ISSUE": {"invoice-issued", "ISSUED", "FAILED"},
}

// CallbackHandler 回调路由：验签后的 CallbackPayload → ActionClient 调用。
// 验签本身由 Gateway.VerifyCallback 完成（W1 门禁），本处理器只做协议翻译。
type CallbackHandler struct {
	Actions ActionClient
	Tasks   TaskStore
	Logf    func(format string, args ...any)
}

func (h *CallbackHandler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

var (
	ErrUnknownEventKey   = errors.New("unknown event key")
	ErrTaskStageNoSignal = errors.New("task stage has no signal mapping")
)

// Handle 执行回调动作：返回 nil 表示动作已投递（API 层幂等，重复回调安全）。
func (h *CallbackHandler) Handle(ctx context.Context, p CallbackPayload) error {
	prefix, arg, err := parseEventKey(p.EventKey)
	if err != nil {
		return err
	}
	switch prefix {
	case "approve", "reject":
		task, err := h.Tasks.GetApprovalTask(ctx, p.TenantID, arg)
		if err != nil {
			return fmt.Errorf("task %s: %w", arg, err)
		}
		if task.OrderID == "" {
			return fmt.Errorf("task %s: no order bound", arg)
		}
		action, ok := stageSignal[task.StageCode+":"+task.Kind]
		if !ok {
			return fmt.Errorf("%w: stage=%s kind=%s", ErrTaskStageNoSignal, task.StageCode, task.Kind)
		}
		verdict := action.okVerdict
		if prefix == "reject" {
			verdict = action.noVerdict
		}
		h.logf("callback: %s task=%s order=%s signal=%s verdict=%s by=%s",
			prefix, arg, task.OrderID, action.signal, verdict, p.FromUserID)
		return h.Actions.OrderSignal(ctx, p.TenantID, task.OrderID, action.signal, p.FromUserID, map[string]any{
			"verdict":  verdict,
			"operator": p.FromUserID,
		})
	case "preorder:confirm", "preorder:ignore", "preorder:modify":
		// 预订单信号：confirm/ignore 为独立信号；「修改后确认」由 preorder-confirm
		// 携带 confirmed=false 表达（工作流 preConfirmStep 语义；api/signals.go 无独立 modify 信号）
		signal := "preorder-" + strings.TrimPrefix(prefix, "preorder:")
		payload := map[string]any{"operator": p.FromUserID}
		switch prefix {
		case "preorder:confirm":
			payload["confirmed"] = true
		case "preorder:modify":
			// 修改后确认 = preorder-confirm + confirmed=false（工作流 preConfirmStep 语义）
			payload["confirmed"] = false
			signal = "preorder-confirm"
		case "preorder:ignore":
			signal = "preorder-ignore"
		}
		h.logf("callback: %s preorder=%s by=%s", prefix, arg, p.FromUserID)
		return h.Actions.PreOrderSignal(ctx, p.TenantID, arg, signal, p.FromUserID, payload)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownEventKey, p.EventKey)
	}
}

// parseEventKey 解析按钮 EventKey（07 §4.2 协议表）：
//
//	approve:<taskID> / reject:<taskID>
//	preorder:confirm:<preOrderID> / preorder:ignore:<preOrderID> / preorder:modify:<preOrderID>
func parseEventKey(key string) (prefix, arg string, err error) {
	parts := strings.Split(key, ":")
	switch {
	case len(parts) == 2 && parts[1] != "" && (parts[0] == "approve" || parts[0] == "reject"):
		return parts[0], parts[1], nil
	case len(parts) == 3 && parts[2] != "" && parts[0] == "preorder" &&
		(parts[1] == "confirm" || parts[1] == "ignore" || parts[1] == "modify"):
		return "preorder:" + parts[1], parts[2], nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnknownEventKey, key)
	}
}
