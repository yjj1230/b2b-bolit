package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
)

// ── OrderWorkflow 主流程（docs/01-core-graph.html §2.1 任务图 + §2.2-2.5 四段状态机）──
//
// 编排纪律：Workflow 只做确定性决策（状态迁移经 domain FSM 校验、Signal 等待、Timer 编排）；
// 所有副作用在 Activity（经 Store/Notifier/AuthChecker 接口注入，L2 用 fake 实现）。
// 人工闸门不可绕过：每个关键节点都等待 Signal（人类确认）才推进。
//
// 实施注记（roundtrip 变更记录见 docs/05）：Shipment/Transfer/ExceptionInvoice 三个子流程
// 以「内联段函数」（shpSegment 内的发货/退回/转仓分支、invSegment 的例外审核分支）实现，
// 状态图语义与设计完全一致；子流程形态（ChildWorkflow）留后续迭代。

// ── Signal 定义（审批动作 = Signal 等待，docs/02 §2.2）──

const (
	SignalPriceReview      = "price-review"
	SignalChairmanReview   = "chairman-review"
	SignalStockReview      = "stock-review"
	SignalWarehouseConfirm = "warehouse-confirm"
	SignalTransferDecision = "transfer-decision"
	SignalPaymentConfirm   = "payment-confirm"
	SignalShipAction       = "ship-action"
	SignalReceiptExReview  = "receipt-ex-review"
	SignalReceiptProvided  = "receipt-provided"
	SignalInvoiceIssued    = "invoice-issued"
	SignalPaymentTracked   = "payment-tracked"
	SignalModifyOrder      = "modify-order"
	SignalResubmit         = "resubmit"
	// SignalShadowInject 影子模式专用控制信号（12 §5：旧系统 timer 触发事件的等价分支注入；
	// 非影子模式投递它没有效果——A 轨行为不变）。
	SignalShadowInject = "shadow-inject"
)

// PriceReviewSignal 主管审价裁决。
type PriceReviewSignal struct {
	Verdict  string // APPROVED / RETURNED / REJECTED
	Operator string
	Reason   string
}

// ChairmanReviewSignal 董事长裁决（C/D 级；DSZ-01：支持 RETURNED「要求修改」）。
type ChairmanReviewSignal struct {
	Verdict  string // APPROVED / RETURNED / REJECTED
	Operator string
}

// StockReviewSignal 商务备货裁决（N4：选备货仓/写备注/通过/驳回）。
type StockReviewSignal struct {
	Verdict     string `json:"verdict"` // APPROVED / RETURNED
	Operator    string `json:"operator"`
	WarehouseID string `json:"warehouse_id"` // 选备货仓（根据收货地址选仓；通过时必填）
	Note        string `json:"note"`         // 备注
}

// WarehouseConfirmSignal 仓管确认裁决。
type WarehouseConfirmSignal struct {
	Verdict  string // APPROVED / RETURNED / TRANSFER
	Operator string
}

// TransferDecisionSignal 商务转仓审核裁决。
type TransferDecisionSignal struct {
	Verdict     string `json:"verdict"` // APPROVE_TRANSFER / APPROVE_ORIGINAL / REJECT
	Operator    string `json:"operator"`
	WarehouseID string `json:"warehouse_id"` // 同意转仓时的目标仓
}

// PaymentConfirmSignal 财务到账确认。
type PaymentConfirmSignal struct {
	Verdict   string // APPROVED / RETURNED
	Operator  string
	LedgerRef string
}

// ShipActionSignal 仓管实际发货动作。
type ShipActionSignal struct {
	Verdict     string `json:"verdict"` // SHIP / RETURN / TRANSFER
	Operator    string `json:"operator"`
	Qty         int64  `json:"qty"`          // SHIP 时的实发数量
	WarehouseID string `json:"warehouse_id"` // TRANSFER 时的目标仓
}

// ReceiptProvidedSignal 补回单（无载荷；分流②/补回单后进入开票）。
type ReceiptProvidedSignal struct{}

// ReceiptExReviewSignal 回单例外审核裁决。
type ReceiptExReviewSignal struct {
	Verdict  string // APPROVED / REJECTED（D-04：APPROVED + DeadlineDays = 限时放行）
	Operator string
	// DeadlineDays D-04：限时放行后补齐回单的期限（天；>0 记入事件供追踪，0=普通放行）
	DeadlineDays int
}

// InvoiceIssuedSignal 财务开票结果。
type InvoiceIssuedSignal struct {
	Verdict  string // ISSUED / FAILED / NO_NEED
	Operator string
	TaxNo    string
}

// PaymentTrackedSignal 回款登记。
type PaymentTrackedSignal struct {
	Amount   int64
	Operator string
}

// ModifyOrderSignal 修改请求（A/B/C/D/E 分类，fig12/13 业务原图）。
type ModifyOrderSignal struct {
	Class    domain.ModificationClass
	Changes  []string
	Operator string
}

// ResubmitSignal 退回后重提。
type ResubmitSignal struct {
	Operator string
}

// ShadowInjectSignal 影子模式等价分支注入（Timer 触发事件 → feed 显式投递）。
type ShadowInjectSignal struct {
	Kind string `json:"kind"` // receipt-exception（无回单超期进例外）
}

// OrderWorkflowParams 主流程参数（ROL-01：审批人路由经 Assignees 配置化）。
type OrderWorkflowParams struct {
	TenantID string
	OrderID  string
	// Timer 时长（测试注入口；生产传 24h/48h/5d，见 config）
	ApprovalRemindAfter   time.Duration
	ApprovalEscalateAfter time.Duration
	ReceiptExceptionAfter time.Duration
	// Shadow 影子模式（12 §5）：true 时 Timer 全部不调度、接受 shadow-inject 分支信号，
	// 推进只认信号不靠真实时间——同一事件序列可确定性重放。A 轨 false，行为不变。
	Shadow bool
	// Assignees 审批人路由（ROL-01 收敛：配置化替换硬编码 u-*；零值 = DefaultAssignees）
	Assignees AssigneeConfig
}

// AssigneeConfig 审批人路由配置（v0.1：由 B2B_ASSIGNEE_* 环境变量注入；测试零值回退默认）。
type AssigneeConfig struct {
	Supervisor        string // 销售主管（审价）
	Chairman          string // 董事长（C/D 终审）
	OP                string // 商务（备货/转仓/例外审核）
	Accounting        string // 财务（到账/开票）
	Sales             string // 销售（D-04：限时放行后补齐回单待办）
	WarehousePrefix   string // 仓管派生前缀（u-wh-<仓库ID>）
	WarehouseFallback string // 未知仓库回退仓管（原硬编码 u-wh-yangyuan）
}

// DefaultAssignees 基线路由（与历史硬编码 u-* 一致，测试/演示零值等价）。
func DefaultAssignees() AssigneeConfig {
	return AssigneeConfig{
		Supervisor: "u-supervisor", Chairman: "u-chairman", OP: "u-op", Accounting: "u-accounting",
		Sales:           "u-sales",
		WarehousePrefix: "u-wh-", WarehouseFallback: "u-wh-yangyuan",
	}
}

func (p OrderWorkflowParams) assignees() AssigneeConfig {
	a := p.Assignees
	if a.Supervisor == "" {
		a.Supervisor = DefaultAssignees().Supervisor
	}
	if a.Chairman == "" {
		a.Chairman = DefaultAssignees().Chairman
	}
	if a.OP == "" {
		a.OP = DefaultAssignees().OP
	}
	if a.Accounting == "" {
		a.Accounting = DefaultAssignees().Accounting
	}
	if a.Sales == "" {
		a.Sales = DefaultAssignees().Sales
	}
	if a.WarehousePrefix == "" {
		a.WarehousePrefix = DefaultAssignees().WarehousePrefix
	}
	if a.WarehouseFallback == "" {
		a.WarehouseFallback = DefaultAssignees().WarehouseFallback
	}
	return a
}

// warehouseAssignee 仓库维度审批人派生（0.1：u-wh-<仓库ID>；未知仓回退默认仓管）。
func (p OrderWorkflowParams) warehouseAssignee(warehouseID string) string {
	a := p.assignees()
	if w := strings.TrimPrefix(warehouseID, "wh-"); w != "" {
		return a.WarehousePrefix + w
	}
	return a.WarehouseFallback
}

// errSegmentRestart 段重启哨兵（修改/重提后从指定段继续）。
type errSegmentRestart struct{ stage string }

func (e *errSegmentRestart) Error() string { return "restart from " + e.stage }

// errFlowEnd 流程终态哨兵（REJECTED/IGNORED/CLOSED 等正常结束）。
var errFlowEnd = errors.New("flow ended")

// OrderWorkflow 主流程：ORD → STK → SHP → INV 四段顺序推进；修改/重提可回退重启。
func OrderWorkflow(ctx workflow.Context, params OrderWorkflowParams) error {
	// 初始段按当前状态推导（支持从中途状态恢复：修改/重提/异常后的重入）
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	if domain.IsTerminal(ord.State) {
		return nil
	}
	stage := initialStage(ord.State)
	for {
		var err error
		switch stage {
		case "", "ORD":
			err = ordSegment(ctx, params)
		case "STK":
			err = stkSegment(ctx, params)
		case "SHP":
			err = shpSegment(ctx, params)
		case "INV":
			err = invSegment(ctx, params)
		default:
			return fmt.Errorf("unknown stage %q", stage)
		}
		var restart *errSegmentRestart
		if errors.As(err, &restart) {
			stage = restart.stage
			continue
		}
		if errors.Is(err, errFlowEnd) {
			return nil
		}
		return err
	}
}

// initialStage 按订单状态推导所在段（重入路径）。
func initialStage(state string) string {
	switch domain.Stage(state) {
	case "STK":
		return "STK"
	case "SHP":
		return "SHP"
	case "INV":
		return "INV"
	}
	return "ORD"
}

// ── 通用工具 ──

// act Activity 执行助手：StartToClose 超时 + 不自动重试（业务守恒类错误需人工/信号驱动
// 重试；临时性基础设施错误由上层调用方策略处理，基线保持确定性）。
func act(ctx workflow.Context, name string, args ...any) workflow.Future {
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	return workflow.ExecuteActivity(actCtx, name, args...)
}

// getOrder 读订单（Activity）。
func getOrder(ctx workflow.Context, params OrderWorkflowParams) (*repo.Order, error) {
	var ord *repo.Order
	err := act(ctx, ActGetOrder, params.TenantID, params.OrderID).Get(ctx, &ord)
	return ord, err
}

// setState 状态迁移（Activity；FSM 校验前置）。
func setState(ctx workflow.Context, tenantID, orderID, from, to, action, operator, reason string) error {
	if err := validateFSM(from, to); err != nil {
		return err
	}
	return act(ctx, ActSetState, StateInput{
		TenantID: tenantID, OrderID: orderID, From: from, To: to,
		Action: action, Operator: operator, Reason: reason,
	}).Get(ctx, nil)
}

// validateFSM 按段校验迁移合法性（防线 6：FSM 表驱动）。
// 按 from 状态所在段选择 FSM（各段 FSM 表内已编码跨段目标边，如 ORD→SHP_SHIP_ASSIGNING）；
// to==ORD_MODIFYING 为修改中断（正交于状态机），允许从任何人工闸门进入。
func validateFSM(from, to string) error {
	if to == domain.StateORDModifying {
		return nil
	}
	switch domain.Stage(from) {
	case "ORD", "":
		return domain.OrdFSM.Validate(from, to)
	case "STK":
		return domain.StkFSM.Validate(from, to)
	case "SHP":
		return domain.ShpFSM.Validate(from, to)
	case "INV":
		return domain.InvFSM.Validate(from, to)
	}
	return domain.PreOrderFSM.Validate(from, to)
}

// awaitGate 等待人工决策：decisionChan 收到即返回；modifyChan 收到修改请求 →
// 先落库应用修改（applyModification）再返回 errSegmentRestart；
// escalateAfter 超时执行 onEscalate 后继续等待。
func awaitGate(ctx workflow.Context, params OrderWorkflowParams, decisionChan workflow.ReceiveChannel,
	receiver func(workflow.ReceiveChannel) any, modifyChan workflow.ReceiveChannel,
	escalateAfter time.Duration, onEscalate func(workflow.Context)) (any, error) {
	for {
		sel := workflow.NewSelector(ctx)
		var got any
		escalated := false
		sel.AddReceive(decisionChan, func(c workflow.ReceiveChannel, more bool) {
			got = receiver(c)
		})
		sel.AddReceive(modifyChan, func(c workflow.ReceiveChannel, more bool) {
			var mod ModifyOrderSignal
			c.Receive(ctx, &mod)
			got = &mod
		})
		if escalateAfter > 0 && !params.Shadow {
			// 影子模式：Timer 全部不调度（推进只认信号，可确定性重放，12 §5/S-06）
			timer := workflow.NewTimer(ctx, escalateAfter)
			sel.AddFuture(timer, func(f workflow.Future) {
				_ = f.Get(ctx, nil)
				escalated = true
				if onEscalate != nil {
					onEscalate(ctx)
				}
			})
		}
		sel.Select(ctx)
		if escalated {
			continue // 催办触发：继续等待人工决策（got 保持 nil 不代表决策）
		}
		if mod, ok := got.(*ModifyOrderSignal); ok {
			stage, err := applyModification(ctx, params, *mod)
			if err != nil {
				return nil, err
			}
			return nil, &errSegmentRestart{stage: stage}
		}
		return got, nil
	}
}

// applyModification 应用 A/B/C/D/E 修改（docs/01-core-graph.html §2.2 分类规则表）：
// 进入 ORD_MODIFYING → resubmit revision 递增 → 按分类跳转到目标状态；
// 返回修改后应重启的段（A/C → ORD；B/D → SHP；E → STK）。
func applyModification(ctx workflow.Context, params OrderWorkflowParams, mod ModifyOrderSignal) (string, error) {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return "", err
	}
	// D-03：已发货/运输中/开票后禁止直接改主订单收货地址（BLOCK → 转物流异常，人工处理）
	if domain.BlockShippedAddressChange(ord.State, mod.Class, mod.Changes) {
		detail := repo.MarshalJSONDetail(map[string]any{"class": string(mod.Class), "changes": mod.Changes, "blocked": true})
		if err := act(ctx, "RecordFlowFailed", params.TenantID, params.OrderID,
			"已发货订单禁止修改收货地址（BLOCK：转物流异常）", detail).Get(ctx, nil); err != nil {
			return "", err
		}
		return initialStage(ord.State), nil
	}
	if err := setState(ctx, params.TenantID, params.OrderID, ord.State, domain.StateORDModifying,
		"ModifyRequested", mod.Operator, "修改请求 分类"+string(mod.Class)); err != nil {
		return "", err
	}
	// resubmit 链：revision 递增（修改即新 revision；A/B/C/D/E 均适用）
	rev := ord.ResubmitRevision + 1
	if err := act(ctx, ActUpdateResubmit, params.TenantID, params.OrderID, rev, ord.ID).Get(ctx, nil); err != nil {
		return "", err
	}
	detail := repo.MarshalJSONDetail(map[string]any{"class": string(mod.Class), "changes": mod.Changes})
	switch mod.Class {
	case domain.ModClassA:
		return "ORD", setState(ctx, params.TenantID, params.OrderID, domain.StateORDModifying,
			domain.StateORDPriceReviewing, "OrderModifiedA", mod.Operator, "A类：回待价格审批重审")
	case domain.ModClassB:
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDModifying,
			domain.StateSHPShipAssigning, "OrderModifiedB", mod.Operator, "B类：账期改非款到直达发货"); err != nil {
			return "", err
		}
		// 直达发货需要隐式备货聚合（镜像非款到路径；0.1 锁库存暂缓，只建聚合不扣库存）
		if err := ensureImplicitStock(ctx, params); err != nil {
			return "", err
		}
		return "SHP", nil
	case domain.ModClassC:
		return "ORD", setState(ctx, params.TenantID, params.OrderID, domain.StateORDModifying,
			domain.StateORDContractGenerate, "OrderModifiedC", mod.Operator, "C类：仅重新生成合同")
	case domain.ModClassD:
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDModifying,
			domain.StateSHPShipAssigning, "OrderModifiedD", mod.Operator, "D类：重生成合同+直达发货"); err != nil {
			return "", err
		}
		if err := ensureImplicitStock(ctx, params); err != nil {
			return "", err
		}
		return "SHP", nil
	case domain.ModClassE:
		// 静默修改：仅更新 payload + 事件（detail 记 changes）
		_ = act(ctx, ActUpdateMeta, params.TenantID, params.OrderID, "", detail).Get(ctx, nil)
		return "STK", setState(ctx, params.TenantID, params.OrderID, domain.StateORDModifying,
			domain.StateORDContracted, "OrderModifiedE", mod.Operator, "E类：静默修改")
	}
	return "", fmt.Errorf("unknown modification class %q", mod.Class)
}

// escalationAction 审批催办（Timer 触发：事件留痕 + 通知；24h 提醒/48h 升级语义由时长参数表达）。
func escalationAction(tenantID, orderID, kind, assignee string) func(workflow.Context) {
	return func(ctx workflow.Context) {
		_ = act(ctx, ActNotifyApproval, tenantID, orderID, kind, assignee, "ESCALATED").Get(ctx, nil)
	}
}

// ensureImplicitStock B/D 修改直达发货的隐式备货聚合（镜像 stkReadyStep 非款到路径；幂等）。
func ensureImplicitStock(ctx workflow.Context, params OrderWorkflowParams) error {
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	sp := repo.StockPrepare{
		ID: "STK-" + shortID(params.OrderID+":1"), TenantID: params.TenantID, OrderID: params.OrderID,
		WarehouseID: "wh-yangyuan", State: domain.StateSTKShipOrderIssued,
		PlannedQty: items[0].Quantity, RemainingQty: items[0].Quantity, BatchSeq: 1,
		IdempotencyKey: "sp:implicit:" + params.OrderID,
	}
	if items[0].WarehouseID != nil {
		sp.WarehouseID = *items[0].WarehouseID
	}
	spItems := []repo.StockPrepareItem{{
		ID: "spi-" + shortID(fmt.Sprintf("%s:%d", params.OrderID, sp.BatchSeq)), TenantID: params.TenantID,
		StockPrepareID: sp.ID, OrderItemID: items[0].ID, ProductID: items[0].ProductID, Qty: items[0].Quantity,
	}}
	return act(ctx, ActReserveStock, sp, spItems, domain.StateSHPShipAssigning,
		domain.StateSHPShipAssigning, "system", "B/D 修改直达发货（隐式备货）", nil).Get(ctx, nil)
}

// ── ORD 段（§2.2）──

func ordSegment(ctx workflow.Context, params OrderWorkflowParams) error {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	modifyChan := workflow.GetSignalChannel(ctx, SignalModifyOrder)
	resubmitChan := workflow.GetSignalChannel(ctx, SignalResubmit)

	// 从当前状态继续（修改/重提后重入）
	switch ord.State {
	case domain.StateORDRequested:
		if err := ordValidateStep(ctx, params, ord, modifyChan, resubmitChan); err != nil {
			return err
		}
	case domain.StateORDPriceReviewing:
		if err := ordPriceStep(ctx, params, ord, modifyChan, resubmitChan); err != nil {
			return err
		}
	case domain.StateORDPriceApproved, domain.StateORDChairmanReview:
		if err := ordGradeStep(ctx, params, ord, modifyChan); err != nil {
			return err
		}
	case domain.StateORDReturned:
		// DSZ-01：董事长「要求修改」后等待销售重提 → 回董事长终审（跳过重复审价）
		for {
			var resub ResubmitSignal
			got := false
			sel := workflow.NewSelector(ctx)
			sel.AddReceive(resubmitChan, func(c workflow.ReceiveChannel, more bool) {
				c.Receive(ctx, &resub)
				got = true
			})
			sel.AddReceive(modifyChan, func(c workflow.ReceiveChannel, more bool) {
				var mod ModifyOrderSignal
				c.Receive(ctx, &mod) // 退回态忽略中途修改：等待显式重提
			})
			sel.Select(ctx)
			if !got {
				continue
			}
			if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDReturned,
				domain.StateORDPriceApproved, "ChairmanResubmitted", resub.Operator, "董事长要求修改后重提"); err != nil {
				return err
			}
			return ordGradeStep(ctx, params, ord, modifyChan)
		}
	case domain.StateORDContractGenerate:
		// C 类修改后重入：重新生成合同
		return ordContractStep(ctx, params, ord)
	case domain.StateORDContracted:
		// 合同完成后直接进入 STK 段（无修改窗口；修改请求在后续人工闸门处处理）
		return &errSegmentRestart{stage: "STK"}
	case domain.StateORDRejected:
		return errFlowEnd // 终态
	}
	return &errSegmentRestart{stage: "STK"}
}

// ordValidateStep 基础校验 → 审价。
func ordValidateStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan, resubmitChan workflow.ReceiveChannel) error {
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	var vdata *ValidateOrderDataResult
	if err := act(ctx, ActValidateOrderData, params.TenantID, ord.CustomerID, items).Get(ctx, &vdata); err != nil {
		return err
	}
	grade := vdata.CustomerGrade
	var vres *ValidateOrderResult
	if err := act(ctx, ActValidateOrder, ord, items, grade).Get(ctx, &vres); err != nil {
		return err
	}
	if !vres.OK {
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateORDRequested, domain.StateORDRejected,
			"OrderRejected", "system", "校验失败: "+vres.Reason)
		return errFlowEnd // 终态
	}
	if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDRequested, domain.StateORDPriceReviewing,
		"PriceReviewStarted", "system", "基础校验通过"); err != nil {
		return err
	}
	return ordPriceStep(ctx, params, ord, modifyChan, resubmitChan)
}

// ordPriceStep 审价闸门。
func ordPriceStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan, resubmitChan workflow.ReceiveChannel) error {
	// 创建审价待办（assignee：销售主管）
	assignee := params.assignees().Supervisor
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "ORD", Kind: "PRICE", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanPrice := workflow.GetSignalChannel(ctx, SignalPriceReview)
	got, err := awaitGate(ctx, params, chanPrice, func(c workflow.ReceiveChannel) any {
		var s PriceReviewSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "PRICE", assignee))
	if err != nil {
		return err
	}
	dec := got.(*PriceReviewSignal)
	switch dec.Verdict {
	case "APPROVED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDPriceReviewing,
			domain.StateORDPriceApproved, "PriceReviewed", dec.Operator, "审价通过"); err != nil {
			return err
		}
		return ordGradeStep(ctx, params, ord, modifyChan)
	case "RETURNED":
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDPriceReviewing,
			domain.StateORDReturned, "PriceReturned", dec.Operator, dec.Reason); err != nil {
			return err
		}
		// 等待重提（ResubmitSignal）→ 回审价
		var resub ResubmitSignal
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(resubmitChan, func(c workflow.ReceiveChannel, more bool) { c.Receive(ctx, &resub) })
		sel.AddReceive(modifyChan, func(c workflow.ReceiveChannel, more bool) {
			var mod ModifyOrderSignal
			c.Receive(ctx, &mod)
		})
		sel.Select(ctx)
		// 修改后重提：RETURNED → PRICE_REVIEWING（设计 §2.2）
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDReturned,
			domain.StateORDPriceReviewing, "PriceResubmitted", resub.Operator, "修改后重提"); err != nil {
			return err
		}
		return ordPriceStep(ctx, params, ord, modifyChan, resubmitChan)
	case "REJECTED":
		decideTask(ctx, params.TenantID, taskID, "REJECTED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDPriceReviewing,
			domain.StateORDRejected, "OrderRejected", dec.Operator, "审价拒绝"); err != nil {
			return err
		}
		return errFlowEnd
	}
	return fmt.Errorf("unknown price verdict %q", dec.Verdict)
}

// ordGradeStep 客户等级分流 → 董事长（C/D）→ 合同生成。
func ordGradeStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// ord 为段入口快照，可能已过期：重读当前状态
	cur, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	ord = cur
	var grade string
	if err := act(ctx, ActGetCustomerGrade, params.TenantID, ord.CustomerID).Get(ctx, &grade); err != nil {
		return err
	}
	from := ord.State
	if from == domain.StateORDPriceApproved {
		if grade == "C" || grade == "D" {
			if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDPriceApproved,
				domain.StateORDChairmanReview, "ChairmanReviewStarted", "system", "C/D 级客户触发董事长审批"); err != nil {
				return err
			}
			assignee := params.assignees().Chairman
			var taskID string
			if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
				TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "ORD", Kind: "CHAIRMAN", AssigneeID: &assignee,
			}).Get(ctx, &taskID); err != nil {
				return err
			}
			chanChair := workflow.GetSignalChannel(ctx, SignalChairmanReview)
			got, err := awaitGate(ctx, params, chanChair, func(c workflow.ReceiveChannel) any {
				var s ChairmanReviewSignal
				c.Receive(ctx, &s)
				return &s
			}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "CHAIRMAN", assignee))
			if err != nil {
				return err
			}
			dec := got.(*ChairmanReviewSignal)
			switch dec.Verdict {
			case "APPROVED":
				decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
				if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDChairmanReview,
					domain.StateORDContractGenerate, "ChairmanReviewed", dec.Operator, "董事长通过"); err != nil {
					return err
				}
			case "REJECTED":
				decideTask(ctx, params.TenantID, taskID, "REJECTED", dec.Operator)
				if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDChairmanReview,
					domain.StateORDRejected, "OrderRejected", dec.Operator, "董事长拒绝"); err != nil {
					return err
				}
				return errFlowEnd
			case "RETURNED":
				// DSZ-01：要求修改 → 退回销售（不终审拒绝），重提后重新走董事长终审
				decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
				if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDChairmanReview,
					domain.StateORDReturned, "ChairmanReturned", dec.Operator, "董事长要求修改"); err != nil {
					return err
				}
				return &errSegmentRestart{stage: "ORD"}
			default:
				return fmt.Errorf("unknown chairman verdict %q", dec.Verdict)
			}
		} else {
			if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDPriceApproved,
				domain.StateORDContractGenerate, "ContractGenerateStarted", "system", "A/B 级跳过董事长"); err != nil {
				return err
			}
		}
	} else if from == domain.StateORDChairmanReview {
		// 修改重入（A 类修改后董事长已通过）
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDChairmanReview,
			domain.StateORDContractGenerate, "ContractGenerateStarted", "system", "董事长已通过"); err != nil {
			return err
		}
	}
	return ordContractStep(ctx, params, ord)
}

// ordContractStep 合同生成（要素不全 → 补全重校验，L2-05）。
func ordContractStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order) error {
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	var customer *pgapi.Customer
	if err := act(ctx, ActGetCustomer, params.TenantID, ord.CustomerID).Get(ctx, &customer); err != nil {
		return err
	}
	in := GenerateContractInput{TenantID: params.TenantID, OrderID: params.OrderID, Order: ord, Items: items, CustomerName: customer.CustomerName}
	var cres *GenerateContractResult
	if err := act(ctx, ActGenerateContract, in).Get(ctx, &cres); err != nil {
		return err
	}
	if !cres.OK {
		// 补全重校验：直接重试（基线要素恒可补全；留事件）
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateORDContractGenerate,
			domain.StateORDContractGenerate, "ContractRetry", "system", "要素不全: "+fmt.Sprint(cres.Missing))
		if err := act(ctx, ActGenerateContract, in).Get(ctx, &cres); err != nil {
			return err
		}
	}
	if err := act(ctx, ActSetContract, params.TenantID, params.OrderID, cres.ContractNo).Get(ctx, nil); err != nil {
		return err
	}
	if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDContractGenerate,
		domain.StateORDContracted, "ContractGenerated", "system", "合同生成校验通过"); err != nil {
		return err
	}
	return &errSegmentRestart{stage: "STK"}
}

// ── STK 段（§2.3）──

func stkSegment(ctx workflow.Context, params OrderWorkflowParams) error {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	modifyChan := workflow.GetSignalChannel(ctx, SignalModifyOrder)

	switch ord.State {
	case domain.StateORDContracted:
		// 设计路径：CONTRACTED → READY_FOR_STOCK → 分流
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateORDContracted,
			domain.StateSTKReadyForStock, "ReadyForStock", "system", "进入备货段"); err != nil {
			return err
		}
		return stkReadyStep(ctx, params, ord, modifyChan)
	case domain.StateSTKReadyForStock:
		return stkReadyStep(ctx, params, ord, modifyChan)
	case domain.StateSTKStockReviewing, domain.StateSTKWarehouseConfirm,
		domain.StateSTKWarehouseReturned, domain.StateSTKWarehouseConfirmed,
		domain.StateSTKPaymentConfirming, domain.StateSTKPaymentConfirmed,
		domain.StateSTKTransferRequested, domain.StateSTKReturned:
		return stkResume(ctx, params, ord, modifyChan)
	case domain.StateSTKShipOrderIssued:
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKShipOrderIssued,
			domain.StateSHPShipAssigning, "ShipAssignStarted", "system", "发货命令已发出"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "SHP"}
	}
	return &errSegmentRestart{stage: "SHP"}
}

// stkReadyStep 备货段入口分流：款到 → 备货审批；非款到 → 隐式备货直达发货分配。
func stkReadyStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	if ord.PaymentTerm == "款到" {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKReadyForStock,
			domain.StateSTKStockReviewing, "StockReviewStarted", "system", "款到发货：商务备货"); err != nil {
			return err
		}
		return stkReserveStep(ctx, params, ord, modifyChan)
	}
	// 非款到：隐式备货（出库单记录，无审批闸门）→ 直达发货分配
	// 备货聚合 ID 与款到显式备货同约定 STK-<order:1>（shpShipStep 的发货单引用同一 ID；
	// 修正：原 :0 与发货引用 :1 不一致，真库下 ShipGoods 找不到备货单）
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	if len(items) > 0 {
		sp := repo.StockPrepare{
			ID: "STK-" + shortID(params.OrderID+":1"), TenantID: params.TenantID, OrderID: params.OrderID,
			WarehouseID: "wh-yangyuan", State: domain.StateSTKShipOrderIssued,
			PlannedQty: items[0].Quantity, RemainingQty: items[0].Quantity, BatchSeq: 1,
			IdempotencyKey: "sp:implicit:" + params.OrderID,
		}
		spItems := []repo.StockPrepareItem{{
			// 明细 ID 含备货批次序号：同一订单多批备货不冲突；修改重入同批次 ID 不变（ON CONFLICT 幂等）
			ID: "spi-" + shortID(fmt.Sprintf("%s:%d", params.OrderID, sp.BatchSeq)), TenantID: params.TenantID, StockPrepareID: sp.ID,
			OrderItemID: items[0].ID, ProductID: items[0].ProductID, Qty: items[0].Quantity,
		}}
		if err := act(ctx, ActReserveStock, sp, spItems, domain.StateSTKReadyForStock,
			domain.StateSHPShipAssigning, "system", "非款到隐式备货（无审批）", nil).Get(ctx, nil); err != nil {
			return err
		}
	}
	return &errSegmentRestart{stage: "SHP"}
}

// stkResume 从 STK 中间状态继续（修改/重提/转仓后重入）。
func stkResume(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	switch ord.State {
	case domain.StateSTKStockReviewing:
		return stkReserveStep(ctx, params, ord, modifyChan)
	case domain.StateSTKWarehouseConfirm:
		// 仓管确认闸门（已建待办则复用）
		return stkWarehouseStep(ctx, params, ord, modifyChan)
	case domain.StateSTKWarehouseReturned:
		// D-02 收敛：仓管退回 → 任务回到商务重新选仓/调交期，重开备货闸门
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseReturned,
			domain.StateSTKStockReviewing, "WarehouseReturnedReplan", "system", "仓管退回→商务重新选仓"); err != nil {
			return err
		}
		return stkReserveStep(ctx, params, ord, modifyChan)
	case domain.StateSTKTransferRequested:
		return stkTransferStep(ctx, params, ord, modifyChan)
	case domain.StateSTKWarehouseConfirmed, domain.StateSTKPaymentConfirming:
		return stkPaymentStep(ctx, params, ord, modifyChan)
	case domain.StateSTKPaymentConfirmed:
		// 财务已确认后重入：直接发发货命令
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKPaymentConfirmed,
			domain.StateSTKShipOrderIssued, "ShipOrderIssued", "system", "重入：发出发货命令"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "SHP"}
	case domain.StateSTKReturned:
		// 退回后重提 → 回备货
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSTKReturned,
			domain.StateSTKStockReviewing, "StockResubmitted", "system", "退回后重提")
		return stkReserveStep(ctx, params, ord, modifyChan)
	}
	return &errSegmentRestart{stage: "SHP"}
}

// stkReserveStep 商务备货闸门（N4：选备货仓/写备注/通过/驳回）→ 备货聚合 → 仓管确认。
// 0.1 A1：备货详情展示收货地址，商务按收货地址选备货仓；锁库存暂不做（本步骤不触碰库存）。
func stkReserveStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("order %s has no items", params.OrderID)
	}
	assignee := params.assignees().OP
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "STK", Kind: "STOCK_REVIEW", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanStock := workflow.GetSignalChannel(ctx, SignalStockReview)
	got, err := awaitGate(ctx, params, chanStock, func(c workflow.ReceiveChannel) any {
		var s StockReviewSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter,
		escalationAction(params.TenantID, params.OrderID, "STOCK_REVIEW", assignee))
	if err != nil {
		return err
	}
	dec := got.(*StockReviewSignal)
	switch dec.Verdict {
	case "APPROVED":
		if err := domain.ValidateWarehouseSelection(dec.Verdict, dec.WarehouseID); err != nil {
			return err
		}
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		// 选仓生效：备货单落到商务所选仓库（收货地址随事件留痕供仓管核对）
		spID := "STK-" + shortID(params.OrderID+":1")
		shipKey, _ := domain.SegmentKey(ord.IdempotencyKey, "ship", 1)
		sp := repo.StockPrepare{
			ID: spID, TenantID: params.TenantID, OrderID: params.OrderID, WarehouseID: dec.WarehouseID,
			State: domain.StateSTKWarehouseConfirm, PlannedQty: items[0].Quantity, RemainingQty: items[0].Quantity,
			BatchSeq: 1, IdempotencyKey: shipKey,
		}
		spItems := []repo.StockPrepareItem{{
			ID: "spi-" + shortID(fmt.Sprintf("%s:%d", params.OrderID, sp.BatchSeq)), TenantID: params.TenantID, StockPrepareID: spID,
			OrderItemID: items[0].ID, ProductID: items[0].ProductID, Qty: items[0].Quantity,
		}}
		detail := repo.MarshalJSONDetail(map[string]any{
			"warehouse_id": dec.WarehouseID, "receive_address": items[0].ReceiveAddress, "note": dec.Note,
		})
		if err := act(ctx, ActReserveStock, sp, spItems, domain.StateSTKStockReviewing,
			domain.StateSTKWarehouseConfirm, dec.Operator, "商务备货通过（选仓）", detail).Get(ctx, nil); err != nil {
			return err
		}
		// 仓管确认待办（本仓维度；0.1 基线仓管用户按仓名派生，未知仓回退阳原仓管）
		whAssignee := params.warehouseAssignee(dec.WarehouseID)
		var whTaskID string
		if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
			TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "STK", Kind: "STOCK_WAREHOUSE", AssigneeID: &whAssignee,
		}).Get(ctx, &whTaskID); err != nil {
			return err
		}
		return stkWarehouseStep(ctx, params, ord, modifyChan)
	case "RETURNED":
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKStockReviewing,
			domain.StateSTKReturned, "StockReturned", dec.Operator, "商务备货驳回: "+dec.Note); err != nil {
			return err
		}
		// 通知销售（XS4 驳回重提）；重入 STK 段重开备货闸门等待重提
		_ = act(ctx, ActNotifyOrderEvent, params.TenantID, params.OrderID, domain.StateSTKReturned).Get(ctx, nil)
		return &errSegmentRestart{stage: "STK"}
	}
	return fmt.Errorf("unknown stock verdict %q", dec.Verdict)
}

// stkWarehouseStep 仓管确认闸门。
func stkWarehouseStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// 重入归一化：WAREHOUSE_RETURNED → WAREHOUSE_CONFIRMING（退回后重提重新确认）
	cur, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	if cur.State == domain.StateSTKWarehouseReturned {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseReturned,
			domain.StateSTKWarehouseConfirm, "WarehouseResubmitted", "system", "退回后重提"); err != nil {
			return err
		}
		cur.State = domain.StateSTKWarehouseConfirm
	}
	ord = cur
	var taskID string
	_ = act(ctx, ActGetPendingTask, params.TenantID, params.OrderID, "STOCK_WAREHOUSE").Get(ctx, &taskID)
	chanWh := workflow.GetSignalChannel(ctx, SignalWarehouseConfirm)
	got, err := awaitGate(ctx, params, chanWh, func(c workflow.ReceiveChannel) any {
		var s WarehouseConfirmSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter,
		escalationAction(params.TenantID, params.OrderID, "STOCK_WAREHOUSE", params.warehouseAssignee("")))
	if err != nil {
		return err
	}
	dec := got.(*WarehouseConfirmSignal)
	switch dec.Verdict {
	case "APPROVED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseConfirm,
			domain.StateSTKWarehouseConfirmed, "WarehouseConfirmed", dec.Operator, "仓管确认可执行"); err != nil {
			return err
		}
		return stkPaymentStep(ctx, params, ord, modifyChan)
	case "RETURNED":
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseConfirm,
			domain.StateSTKWarehouseReturned, "WarehouseReturned", dec.Operator, "仓管退回"); err != nil {
			return err
		}
		// D-02 收敛：重启 STK 段 → stkResume 将退回转为商务重新选仓闸门
		return &errSegmentRestart{stage: "STK"}
	case "TRANSFER":
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseConfirm,
			domain.StateSTKTransferRequested, "TransferRequested", dec.Operator, "仓管申请转仓"); err != nil {
			return err
		}
		return stkTransferStep(ctx, params, ord, modifyChan)
	}
	return fmt.Errorf("unknown warehouse verdict %q", dec.Verdict)
}

// stkTransferStep 转仓审核闸门（商务）。
func stkTransferStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	var taskID string
	_ = act(ctx, ActGetPendingTask, params.TenantID, params.OrderID, "TRANSFER").Get(ctx, &taskID)
	chanTr := workflow.GetSignalChannel(ctx, SignalTransferDecision)
	got, err := awaitGate(ctx, params, chanTr, func(c workflow.ReceiveChannel) any {
		var s TransferDecisionSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter,
		escalationAction(params.TenantID, params.OrderID, "TRANSFER", params.assignees().OP))
	if err != nil {
		return err
	}
	dec := got.(*TransferDecisionSignal)
	switch dec.Verdict {
	case "APPROVE_TRANSFER":
		// 换仓重确认（基线：更新备货仓；详情留事件）
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSTKTransferRequested,
			domain.StateSTKWarehouseConfirm, "TransferApproved", dec.Operator, "同意转仓: "+dec.WarehouseID)
		return stkWarehouseStep(ctx, params, ord, modifyChan)
	case "APPROVE_ORIGINAL":
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSTKTransferRequested,
			domain.StateSTKWarehouseConfirmed, "TransferRejectedOriginal", dec.Operator, "同意原仓")
		return stkPaymentStep(ctx, params, ord, modifyChan)
	case "REJECT":
		return setState(ctx, params.TenantID, params.OrderID, domain.StateSTKTransferRequested,
			domain.StateSTKReturned, "TransferRejected", dec.Operator, "驳回转仓→退回修改")
	}
	return fmt.Errorf("unknown transfer verdict %q", dec.Verdict)
}

// stkPaymentStep 财务到账闸门（款到发货；非款到跳过）。
func stkPaymentStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// 重入归一化：重读当前状态（PAYMENT_CONFIRMING 启动/修改重入时不再重复迁移）
	cur, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	ord = cur
	if ord.PaymentTerm != "款到" {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseConfirmed,
			domain.StateSTKShipOrderIssued, "ShipOrderIssued", "system", "非款到：跳过财务确认"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "SHP"}
	}
	if ord.State == domain.StateSTKWarehouseConfirmed {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKWarehouseConfirmed,
			domain.StateSTKPaymentConfirming, "PaymentConfirmStarted", "system", "款到发货：财务确认到账"); err != nil {
			return err
		}
	}
	assignee := params.assignees().Accounting
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "STK", Kind: "PAYMENT", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanPay := workflow.GetSignalChannel(ctx, SignalPaymentConfirm)
	got, err := awaitGate(ctx, params, chanPay, func(c workflow.ReceiveChannel) any {
		var s PaymentConfirmSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "PAYMENT", assignee))
	if err != nil {
		return err
	}
	dec := got.(*PaymentConfirmSignal)
	switch dec.Verdict {
	case "APPROVED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKPaymentConfirming,
			domain.StateSTKPaymentConfirmed, "PaymentConfirmed", dec.Operator, "财务确认到账"); err != nil {
			return err
		}
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKPaymentConfirmed,
			domain.StateSTKShipOrderIssued, "ShipOrderIssued", dec.Operator, "商务发出发货命令"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "SHP"}
	case "RETURNED":
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		return setState(ctx, params.TenantID, params.OrderID, domain.StateSTKPaymentConfirming,
			domain.StateSTKReturned, "PaymentReturned", dec.Operator, "未到账→退回商务")
	}
	return fmt.Errorf("unknown payment verdict %q", dec.Verdict)
}

// ── SHP 段（§2.4）──

func shpSegment(ctx workflow.Context, params OrderWorkflowParams) error {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	modifyChan := workflow.GetSignalChannel(ctx, SignalModifyOrder)
	switch ord.State {
	case domain.StateSTKShipOrderIssued:
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSTKShipOrderIssued,
			domain.StateSHPShipAssigning, "ShipAssignStarted", "system", "发货命令已发出"); err != nil {
			return err
		}
		return shpAssignStep(ctx, params, ord, modifyChan)
	case domain.StateSHPShipAssigning, domain.StateSHPWarehouseShip:
		return shpAssignStep(ctx, params, ord, modifyChan)
	case domain.StateSHPPartialShipped:
		return shpContinueStep(ctx, params, ord, modifyChan)
	case domain.StateSHPFullyShipped:
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSHPFullyShipped,
			domain.StateINVReadyForInvoice, "ReadyForInvoice", "system", "全部发货完成"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "INV"}
	case domain.StateSHPShipPed:
		return shpCompletionStep(ctx, params, ord, modifyChan)
	case domain.StateSTKWarehouseConfirm:
		// 部分发货后退回 STK 段（转仓/退回）
		return &errSegmentRestart{stage: "STK"}
	}
	return fmt.Errorf("SHP segment: unexpected state %s", ord.State)
}

// shpAssignStep 发货分配闸门（商务分配 → 仓管实发）。
func shpAssignStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// 重入归一化：WAREHOUSE_SHIPPING → SHIP_ASSIGNING（等待新的实发指令）
	if ord.State == domain.StateSHPWarehouseShip {
		cur, err := getOrder(ctx, params)
		if err != nil {
			return err
		}
		if cur.State == domain.StateSHPWarehouseShip {
			if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSHPWarehouseShip,
				domain.StateSHPShipAssigning, "ShipAssignResumed", "system", "重入发货分配"); err != nil {
				return err
			}
		}
		ord = cur
	}
	assignee := params.warehouseAssignee("")
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "SHP", Kind: "WAREHOUSE", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanShip := workflow.GetSignalChannel(ctx, SignalShipAction)
	got, err := awaitGate(ctx, params, chanShip, func(c workflow.ReceiveChannel) any {
		var s ShipActionSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "WAREHOUSE", assignee))
	if err != nil {
		return err
	}
	dec := got.(*ShipActionSignal)
	switch dec.Verdict {
	case "SHIP":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		return shpShipStep(ctx, params, ord, dec, modifyChan)
	case "RETURN":
		// 少发回退备货层（回到分配，重新提量）
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSHPShipAssigning,
			domain.StateSHPShipAssigning, "ShipReturned", dec.Operator, "仓管退回（少发）")
		return shpAssignStep(ctx, params, ord, modifyChan)
	case "TRANSFER":
		// 回 STK 段转仓
		decideTask(ctx, params.TenantID, taskID, "RETURNED", dec.Operator)
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSHPShipAssigning,
			domain.StateSTKWarehouseConfirm, "TransferRequested", dec.Operator, "仓管申请转仓（SHP）")
		return &errSegmentRestart{stage: "STK"}
	}
	return fmt.Errorf("unknown ship verdict %q", dec.Verdict)
}

// shpShipStep 实际发货（发货+库存扣减+状态迁移单事务）。
func shpShipStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, dec *ShipActionSignal, modifyChan workflow.ReceiveChannel) error {
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("order %s has no items", params.OrderID)
	}
	// 部分发货序号：按既有发货单数派生（幂等键含段序号，CQ-14 重放安全）
	var shipCount int
	if err := act(ctx, ActCountShipments, params.TenantID, params.OrderID).Get(ctx, &shipCount); err != nil {
		return err
	}
	seq := shipCount + 1
	shipID := "SHP-" + shortID(fmt.Sprintf("%s:ship:%d", params.OrderID, seq))
	shipKey, _ := domain.SegmentKey(ord.IdempotencyKey, "ship", seq)
	spID := "STK-" + shortID(params.OrderID+":1")
	// 0.1 选仓生效（N-03）：发货单继承商务所选备货仓；备货单缺失回退阳原仓
	warehouseID := "wh-yangyuan"
	var sp *repo.StockPrepare
	if err := act(ctx, ActGetStockPrepare, params.TenantID, spID).Get(ctx, &sp); err == nil && sp != nil {
		warehouseID = sp.WarehouseID
	}
	ship := repo.Shipment{
		ID: shipID, TenantID: params.TenantID, OrderID: params.OrderID,
		StockPrepareID: spID, WarehouseID: warehouseID,
		State: domain.ShipmentStateShipped, PlannedQty: items[0].Quantity, ShippedQty: dec.Qty,
		BatchSeq: seq, IdempotencyKey: shipKey,
	}
	shipItems := []repo.ShipmentItem{{
		// 明细 ID 含段序号：部分发货多张发货单同订单明细不冲突（修正：原仅按订单派生，第二批 PK 冲突）
		ID: "si-" + shortID(fmt.Sprintf("%s:ship:%d", params.OrderID, seq)), TenantID: params.TenantID,
		ShipmentID: shipID, OrderItemID: items[0].ID, ProductID: items[0].ProductID, Qty: dec.Qty,
	}}
	if err := act(ctx, ActShipGoods, ship, shipItems,
		domain.StateSHPShipAssigning, domain.StateSHPShipPed, dec.Operator, "实际发货", nil).Get(ctx, nil); err != nil {
		return err
	}
	_ = act(ctx, ActNotifyOrderEvent, params.TenantID, params.OrderID, domain.StateSHPShipPed).Get(ctx, nil)
	return shpCompletionStep(ctx, params, ord, modifyChan)
}

// shpCompletionStep 累计发货 → 全部/部分发货判定。
func shpCompletionStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	var shipped map[string]int64
	if err := act(ctx, ActSumShipped, params.TenantID, params.OrderID).Get(ctx, &shipped); err != nil {
		return err
	}
	var items []repo.OrderItem
	if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
		return err
	}
	total := int64(0)
	for _, it := range items {
		total += shipped[it.ID]
	}
	orderQty := int64(0)
	for _, it := range items {
		orderQty += it.Quantity
	}
	if domain.ShipCompletion(total, orderQty) {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSHPShipPed,
			domain.StateSHPFullyShipped, "FullyShipped", "system", "累计发货=订单量"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "INV"}
	}
	if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSHPShipPed,
		domain.StateSHPPartialShipped, "PartialShipped", "system", "累计发货<订单量"); err != nil {
		return err
	}
	return shpContinueStep(ctx, params, ord, modifyChan)
}

// shpContinueStep 部分发货后继续（按原计划继续 / 退回转仓 / 已发部分申请开票）。
func shpContinueStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	var taskID string
	_ = act(ctx, ActGetPendingTask, params.TenantID, params.OrderID, "WAREHOUSE").Get(ctx, &taskID)
	chanShip := workflow.GetSignalChannel(ctx, SignalShipAction)
	got, err := awaitGate(ctx, params, chanShip, func(c workflow.ReceiveChannel) any {
		var s ShipActionSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter,
		escalationAction(params.TenantID, params.OrderID, "WAREHOUSE", params.warehouseAssignee("")))
	if err != nil {
		return err
	}
	dec := got.(*ShipActionSignal)
	switch dec.Verdict {
	case "SHIP":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSHPPartialShipped,
			domain.StateSHPShipAssigning, "ShipContinue", dec.Operator, "按原分批计划继续")
		return shpShipStep(ctx, params, ord, dec, modifyChan)
	case "TRANSFER":
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateSHPPartialShipped,
			domain.StateSTKWarehouseConfirm, "TransferRequested", dec.Operator, "仓管申请转仓（部分）")
		return &errSegmentRestart{stage: "STK"}
	case "INVOICE":
		// 已发部分申请开票
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateSHPPartialShipped,
			domain.StateINVReadyForInvoice, "ReadyForInvoice", dec.Operator, "已发部分申请开票"); err != nil {
			return err
		}
		return &errSegmentRestart{stage: "INV"}
	}
	return fmt.Errorf("unknown ship verdict %q", dec.Verdict)
}

// ── INV 段（§2.5）──

func invSegment(ctx workflow.Context, params OrderWorkflowParams) error {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	modifyChan := workflow.GetSignalChannel(ctx, SignalModifyOrder)
	switch ord.State {
	case domain.StateSHPFullyShipped, domain.StateSHPPartialShipped:
		// SHP→INV 边界：发货完成进入开票段
		if err := setState(ctx, params.TenantID, params.OrderID, ord.State,
			domain.StateINVReadyForInvoice, "ReadyForInvoice", "system", "发货完成进入开票段"); err != nil {
			return err
		}
		return invReceiptStep(ctx, params, ord, modifyChan)
	case domain.StateINVReadyForInvoice:
		return invReceiptStep(ctx, params, ord, modifyChan)
	case domain.StateINVReceiptExReviewing, domain.StateINVWaitingReceipt,
		domain.StateINVInvoiceApplying, domain.StateINVInvoicing, domain.StateINVInvoiceFailed:
		return invInvoiceStep(ctx, params, ord, modifyChan)
	case domain.StateINVInvoiced, domain.StateINVNoInvoiceNeeded, domain.StateINVPaymentTracking:
		return invPaymentStep(ctx, params)
	case domain.StateINVContractArchived:
		return setState(ctx, params.TenantID, params.OrderID, domain.StateINVContractArchived,
			domain.StateINVClosed, "OrderClosed", "system", "合同归档完成")
	case domain.StateINVClosed:
		return nil // 终态
	}
	return nil
}

// invReceiptStep 回单四分流（款到/有回单 → 直接；无回单超时/提前 → 例外审核）。
func invReceiptStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	if ord.PaymentTerm == "款到" {
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVReadyForInvoice,
			domain.StateINVInvoiceApplying, "InvoiceApplyStarted", "system", "分流①款到发货直接进入"); err != nil {
			return err
		}
		return invInvoiceStep(ctx, params, ord, modifyChan)
	}
	// 无回单：等待回单 / 5 天 Timer（例外分流）；影子模式等待 shadow-inject 等价分支注入
	chanReceipt := workflow.GetSignalChannel(ctx, SignalReceiptProvided)
	chanInject := workflow.GetSignalChannel(ctx, SignalShadowInject)
	type gate struct{ kind string }
	for {
		sel := workflow.NewSelector(ctx)
		g := &gate{}
		sel.AddReceive(chanReceipt, func(c workflow.ReceiveChannel, more bool) { c.Receive(ctx, nil); g.kind = "receipt" })
		sel.AddReceive(modifyChan, func(c workflow.ReceiveChannel, more bool) {
			var mod ModifyOrderSignal
			c.Receive(ctx, &mod)
			g.kind = "modify"
		})
		if params.Shadow {
			sel.AddReceive(chanInject, func(c workflow.ReceiveChannel, more bool) {
				var inj ShadowInjectSignal
				c.Receive(ctx, &inj)
				if inj.Kind == "receipt-exception" {
					g.kind = "timer" // 等价于真实 Timer 触发
				} else {
					g.kind = "unknown-inject" // 未知注入：忽略并继续等待（不推进、不重启）
				}
			})
		} else if params.ReceiptExceptionAfter > 0 {
			timer := workflow.NewTimer(ctx, params.ReceiptExceptionAfter)
			sel.AddFuture(timer, func(f workflow.Future) { _ = f.Get(ctx, nil); g.kind = "timer" })
		}
		sel.Select(ctx)
		switch g.kind {
		case "unknown-inject":
			continue
		case "receipt":
			if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVReadyForInvoice,
				domain.StateINVInvoiceApplying, "ReceiptProvided", "system", "分流②补回单后进入开票"); err != nil {
				return err
			}
			return invInvoiceStep(ctx, params, ord, modifyChan)
		case "timer":
			_ = setState(ctx, params.TenantID, params.OrderID, domain.StateINVReadyForInvoice,
				domain.StateINVReceiptExReviewing, "ReceiptException", "system", "分流③无回单超5天")
			return invExceptionStep(ctx, params, ord, modifyChan)
		default:
			return &errSegmentRestart{stage: "ORD"}
		}
	}
}

// invExceptionStep 回单例外审核（ExceptionInvoice 子流程的内联实现）。
func invExceptionStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	assignee := params.assignees().OP
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "INV", Kind: "RECEIPT_EX", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanEx := workflow.GetSignalChannel(ctx, SignalReceiptExReview)
	got, err := awaitGate(ctx, params, chanEx, func(c workflow.ReceiveChannel) any {
		var s ReceiptExReviewSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "RECEIPT_EX", assignee))
	if err != nil {
		return err
	}
	dec := got.(*ReceiptExReviewSignal)
	switch dec.Verdict {
	case "APPROVED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		reason := "例外审核通过"
		if dec.DeadlineDays > 0 {
			reason = fmt.Sprintf("例外审核通过（限时放行：%d 天补齐回单）", dec.DeadlineDays)
			// D-04：给销售生成「补齐回单」待办（清单可见；回单补齐后由凭证链闭合）
			sales := params.assignees().Sales
			var followID string
			if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
				TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "INV", Kind: "RECEIPT_FOLLOWUP", AssigneeID: &sales,
			}).Get(ctx, &followID); err != nil {
				return err
			}
			_ = followID
		}
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVReceiptExReviewing,
			domain.StateINVInvoiceApplying, "ReceiptExceptionApproved", dec.Operator, reason); err != nil {
			return err
		}
		return invInvoiceStep(ctx, params, ord, modifyChan)
	case "REJECTED":
		decideTask(ctx, params.TenantID, taskID, "REJECTED", dec.Operator)
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateINVReceiptExReviewing,
			domain.StateINVWaitingReceipt, "ReceiptExceptionRejected", dec.Operator, "例外拒绝→待补回单")
		// 等补回单 → 进入开票
		chanReceipt := workflow.GetSignalChannel(ctx, SignalReceiptProvided)
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(chanReceipt, func(c workflow.ReceiveChannel, more bool) { c.Receive(ctx, nil) })
		sel.AddReceive(modifyChan, func(c workflow.ReceiveChannel, more bool) {
			var mod ModifyOrderSignal
			c.Receive(ctx, &mod)
		})
		sel.Select(ctx)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVWaitingReceipt,
			domain.StateINVInvoiceApplying, "ReceiptProvided", dec.Operator, "补回单后进入开票"); err != nil {
			return err
		}
		return invInvoiceStep(ctx, params, ord, modifyChan)
	}
	return fmt.Errorf("unknown receipt-ex verdict %q", dec.Verdict)
}

// invInvoiceStep 开票申请 → 财务开票（守恒校验；失败可重试；无需开票分流）。
func invInvoiceStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// ord 为段入口快照：重读当前状态
	cur, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	ord = cur
	from := ord.State
	switch from {
	case domain.StateINVReceiptExReviewing, domain.StateINVWaitingReceipt, domain.StateINVReadyForInvoice:
		// 先进入 INVOICE_APPLYING（合法迁移：例外通过/补回单/分流后进入开票申请）
		if err := setState(ctx, params.TenantID, params.OrderID, from,
			domain.StateINVInvoiceApplying, "InvoiceApplyStarted", "system", "进入开票申请"); err != nil {
			return err
		}
		from = domain.StateINVInvoiceApplying
	case domain.StateINVInvoiceFailed:
		// 失败重试：FAILED → INVOICING
		if err := setState(ctx, params.TenantID, params.OrderID, from,
			domain.StateINVInvoicing, "InvoiceRetry", "system", "重试开票"); err != nil {
			return err
		}
		from = domain.StateINVInvoicing
	}
	if from == domain.StateINVInvoiceApplying {
		if err := setState(ctx, params.TenantID, params.OrderID, from,
			domain.StateINVInvoicing, "InvoiceSubmitStarted", "system", "提交开票申请→推送财务"); err != nil {
			return err
		}
	}
	assignee := params.assignees().Accounting
	var taskID string
	if err := act(ctx, ActCreateApprovalTask, repo.NewApprovalTask{
		TenantID: params.TenantID, OrderID: params.OrderID, StageCode: "INV", Kind: "INVOICE_FINANCE_ISSUE", AssigneeID: &assignee,
	}).Get(ctx, &taskID); err != nil {
		return err
	}
	chanInv := workflow.GetSignalChannel(ctx, SignalInvoiceIssued)
	got, err := awaitGate(ctx, params, chanInv, func(c workflow.ReceiveChannel) any {
		var s InvoiceIssuedSignal
		c.Receive(ctx, &s)
		return &s
	}, modifyChan, params.ApprovalRemindAfter, escalationAction(params.TenantID, params.OrderID, "INVOICE_FINANCE_ISSUE", assignee))
	if err != nil {
		return err
	}
	dec := got.(*InvoiceIssuedSignal)
	switch dec.Verdict {
	case "ISSUED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		// 开票量 = 有效发货量（设计：开票按有效发货核验，CQ-09）；守恒（金额+数量双维）
		// 在 Activity 事务内校验；失败 → 拒绝并补偿（L2-10）
		var items []repo.OrderItem
		if err := act(ctx, ActGetOrderItems, params.TenantID, params.OrderID).Get(ctx, &items); err != nil {
			return err
		}
		if len(items) == 0 {
			return fmt.Errorf("order %s has no items", params.OrderID)
		}
		var shipped map[string]int64
		if err := act(ctx, ActSumShipped, params.TenantID, params.OrderID).Get(ctx, &shipped); err != nil {
			return err
		}
		invoiceQty := shipped[items[0].ID]
		if invoiceQty <= 0 {
			return fmt.Errorf("order %s has no effective shipment to invoice", params.OrderID)
		}
		invID := "INV-" + shortID(params.OrderID+":inv")
		invKey, _ := domain.SegmentKey(ord.IdempotencyKey, "inv", 1)
		amount := items[0].UnitPriceCents * invoiceQty
		inv := repo.Invoice{
			ID: invID, TenantID: params.TenantID, OrderID: params.OrderID,
			State: domain.InvoiceStateIssued, AmountCents: amount, Qty: invoiceQty,
			IdempotencyKey: invKey,
		}
		invItems := []repo.InvoiceItem{{
			ID: "ii-" + shortID(params.OrderID), TenantID: params.TenantID, InvoiceID: invID,
			OrderItemID: items[0].ID, ProductID: items[0].ProductID,
			Qty: invoiceQty, AmountCents: amount, IdempotencyKey: invKey + ":1",
		}}
		if err := act(ctx, ActApplyInvoice, inv, invItems,
			domain.StateINVInvoicing, domain.StateINVInvoiced, dec.Operator, "开票完成", nil).Get(ctx, nil); err != nil {
			// 守恒失败 → 状态回退失败态（可重试）
			_ = setState(ctx, params.TenantID, params.OrderID, domain.StateINVInvoicing,
				domain.StateINVInvoiceFailed, "InvoiceFailed", dec.Operator, "开票守恒校验失败")
			return invInvoiceRetryStep(ctx, params, ord, modifyChan)
		}
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVInvoiced,
			domain.StateINVPaymentTracking, "PaymentTrackingStarted", dec.Operator, "开票完成→回款跟踪"); err != nil {
			return err
		}
		return invPaymentStep(ctx, params)
	case "FAILED":
		decideTask(ctx, params.TenantID, taskID, "REJECTED", dec.Operator)
		_ = setState(ctx, params.TenantID, params.OrderID, domain.StateINVInvoicing,
			domain.StateINVInvoiceFailed, "InvoiceFailed", dec.Operator, "开票失败（可重试）")
		return invInvoiceRetryStep(ctx, params, ord, modifyChan)
	case "NO_NEED":
		decideTask(ctx, params.TenantID, taskID, "APPROVED", dec.Operator)
		if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVInvoicing,
			domain.StateINVNoInvoiceNeeded, "NoInvoiceNeeded", dec.Operator, "无需开票"); err != nil {
			return err
		}
		return invPaymentStep(ctx, params)
	}
	return fmt.Errorf("unknown invoice verdict %q", dec.Verdict)
}

// invInvoiceRetryStep 开票失败重试（INVOICE_FAILED → INVOICING）。
func invInvoiceRetryStep(ctx workflow.Context, params OrderWorkflowParams, ord *repo.Order, modifyChan workflow.ReceiveChannel) error {
	// 重入：invInvoiceStep 处理 FAILED → INVOICING 后继续等待财务信号
	ord.State = domain.StateINVInvoiceFailed
	return invInvoiceStep(ctx, params, ord, modifyChan)
}

// invPaymentStep 回款跟踪 → 合同归档 → 订单关闭。
func invPaymentStep(ctx workflow.Context, params OrderWorkflowParams) error {
	ord, err := getOrder(ctx, params)
	if err != nil {
		return err
	}
	if ord.State == domain.StateINVInvoiced || ord.State == domain.StateINVNoInvoiceNeeded {
		if err := setState(ctx, params.TenantID, params.OrderID, ord.State,
			domain.StateINVPaymentTracking, "PaymentTrackingStarted", "system", "回款跟踪"); err != nil {
			return err
		}
	}
	chanPay := workflow.GetSignalChannel(ctx, SignalPaymentTracked)
	var pt PaymentTrackedSignal
	for {
		sel := workflow.NewSelector(ctx)
		done := false
		sel.AddReceive(chanPay, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, &pt)
			done = true
		})
		if params.ApprovalRemindAfter > 0 && !params.Shadow {
			// 回款跟踪静默提醒（keep-alive timer：测试环境虚拟时钟依赖挂起 timer 阻断
			// workflow-timeout 自动触发；生产语义 = 回款催办提醒）；影子模式不调度。
			timer := workflow.NewTimer(ctx, params.ApprovalRemindAfter)
			sel.AddFuture(timer, func(f workflow.Future) { _ = f.Get(ctx, nil) })
		}
		sel.Select(ctx)
		if done {
			break
		}
	}
	if err := setState(ctx, params.TenantID, params.OrderID, domain.StateINVPaymentTracking,
		domain.StateINVContractArchived, "ContractArchived", pt.Operator, fmt.Sprintf("回款登记 ¥%d", pt.Amount)); err != nil {
		return err
	}
	return setState(ctx, params.TenantID, params.OrderID, domain.StateINVContractArchived,
		domain.StateINVClosed, "OrderClosed", pt.Operator, "合同归档完成")
}

// decideTask 审批待办裁决落库（best-effort，状态推进主证据在事件链）。
func decideTask(ctx workflow.Context, tenantID, taskID, toState, operator string) {
	if taskID != "" {
		_ = act(ctx, ActDecideTask, tenantID, taskID, toState, toState, operator, nil).Get(ctx, nil)
	}
}

// shortID 确定性短 ID（与 repo 同源派生，保证工作流内生成的 ID 与事务写入一致）。
func shortID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// StateInput 状态迁移 Activity 入参。
type StateInput struct {
	TenantID string
	OrderID  string
	From     string
	To       string
	Action   string
	Operator string
	Reason   string
}

// SetState 状态迁移 Activity（ActSetState）。
func (a *Activities) SetState(ctx context.Context, in StateInput) error {
	return a.Store.UpdateOrderStateTx(ctx, in.TenantID, in.OrderID, in.From, in.To, in.Action, in.Operator, in.Reason, nil)
}

// SetContract 合同号写入 Activity（ActSetContract）。
func (a *Activities) SetContract(ctx context.Context, tenantID, orderID, contractNo string) error {
	return a.Store.SetOrderContractTx(ctx, tenantID, orderID, contractNo)
}

// UpdateResubmitChain 修改产生的 resubmit 链：revision 递增 + from 链（ActUpdateResubmit）。
func (a *Activities) UpdateResubmitChain(ctx context.Context, tenantID, orderID string, revision int, fromOrderID string) error {
	return a.Store.UpdateResubmitChainTx(ctx, tenantID, orderID, revision, fromOrderID)
}

// UpdateOrderMeta 静默修改：更新 payload（ActUpdateMeta）。
func (a *Activities) UpdateOrderMeta(ctx context.Context, tenantID, orderID, contractNo string, payload []byte) error {
	return a.Store.UpdateOrderMetaTx(ctx, tenantID, orderID, contractNo, payload)
}

// GetPendingTask 取指定 kind 的 PENDING 待办（闸门裁决落库用）。
func (a *Activities) GetPendingTask(ctx context.Context, tenantID, orderID, kind string) (string, error) {
	tasks, err := a.Store.ListApprovalTasks(ctx, tenantID, "", kind, "PENDING", 1)
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return "", nil
	}
	return tasks[0].ID, nil
}

// CountShipments 统计订单发货单数（部分发货序号派生；ActCountShipments）。
func (a *Activities) CountShipments(ctx context.Context, tenantID, orderID string) (int, error) {
	return a.Store.CountShipments(ctx, tenantID, orderID)
}

// SumShippedActivityName 聚合发货 Activity 名。
const (
	ActSetState    = "SetState"
	ActSetContract = "SetContract"
	ActSumShipped  = "SumShipped"
)
