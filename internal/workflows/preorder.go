package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── PreOrderWorkflow（docs/01-core-graph.html §2.6 预订单状态机；企微 S2）──
//
// AI 安全边界：LLM 只产候选（ParseIntent），确定性校验裁决；任何生成结果都需
// 人工确认（PENDING_CONFIRM → CONFIRMED/MODIFIED）才能派生正式订单；
// 低置信 → AMBIGUOUS 只发轻提醒不自动建单；24h 未确认 → Timer 升级（ESCALATED）。

const (
	SignalPreOrderConfirm = "preorder-confirm"
	SignalPreOrderIgnore  = "preorder-ignore"
)

// PreOrderIgnoreSignal 销售忽略（无载荷）。
type PreOrderIgnoreSignal struct{}

// PreOrderConfirmSignal 销售确认（卡片按钮 → Signal）。
type PreOrderConfirmSignal struct {
	Confirmed bool
	Operator  string
	// DraftVersion 卡片对应的不可变草稿版本；0 仅兼容旧卡片，生产请求必须携带。
	DraftVersion int64 `json:"draft_version,omitempty"`
	// 修改后数量（MODIFIED：销售修改后确认）
	ModifiedQuantity int64
	// 修改后单价（分）
	ModifiedUnitPriceCents int64
}

// PreOrderWorkflowParams 预订单工作流参数。
type PreOrderWorkflowParams struct {
	TenantID   string
	PreOrderID string
	// 消息发送者（07 §3.2 权限校验：发送者 = 归属销售/代办（SpiceDB），失败 → 忽略 + 审计）
	SenderID string
	// 24h 升级 Timer（测试注入口；生产传 24h）
	EscalateAfter time.Duration
	// Shadow 影子模式（12 §5/S-06）：Timer 不调度，推进只认信号（与 OrderWorkflow 同口径）。
	Shadow bool
}

// PreOrderWorkflow 消息 → 预订单 → 确认 → 派生正式订单（双层幂等：msgid + 订单键）。
func PreOrderWorkflow(ctx workflow.Context, params PreOrderWorkflowParams) error {
	// 初始：CAPTURED → INTENT_PARSING（仅首次；重放幂等）
	var po *repo.PreOrder
	if err := act(ctx, ActGetPreOrder, params.TenantID, params.PreOrderID).Get(ctx, &po); err != nil {
		return err
	}
	modifyChan := workflow.GetSignalChannel(ctx, SignalModifyOrder)
	_ = modifyChan // 预订单不处理订单修改信号（不同域）

	switch po.State {
	case domain.StatePreCaptured:
		if err := setPreState(ctx, params, domain.StatePreCaptured, domain.StatePreIntentParsing,
			"IntentParsing", "意图识别（LLM 候选+确定性校验）", po.Candidates, ""); err != nil {
			return err
		}
		return preIntentStep(ctx, params)
	case domain.StatePreIntentParsing:
		return preIntentStep(ctx, params)
	case domain.StatePrePendingConfirm, domain.StatePreAmbiguous, domain.StatePreEscalated:
		return preConfirmStep(ctx, params, po)
	case domain.StatePreOrderRequested, domain.StatePreConfirmed, domain.StatePreModified:
		// 幂等重放：确认已完成
		return nil
	case domain.StatePreIgnored:
		return nil // 终态
	}
	return fmt.Errorf("preorder %s in unknown state %s", params.PreOrderID, po.State)
}

// preIntentStep 意图解析：候选 → 高置信/低置信分流。
func preIntentStep(ctx workflow.Context, params PreOrderWorkflowParams) error {
	var po *repo.PreOrder
	if err := act(ctx, ActGetPreOrder, params.TenantID, params.PreOrderID).Get(ctx, &po); err != nil {
		return err
	}
	var ires *ParseIntentResult
	if err := act(ctx, ActParseIntent, params.TenantID, po.Candidates).Get(ctx, &ires); err != nil {
		return err
	}
	// 07 §3.2 权限校验：消息发送者必须是客户归属销售/代办（SpiceDB 唯一判定点）；
	// 失败 → 忽略 + 审计（不自动建单，AI 安全边界）。
	// 0.1 A7/N-04：客户由群上下文带出（候选 customer_id），归属锁定。
	if params.SenderID != "" {
		var authorized bool
		if err := act(ctx, ActAuthCheck, AuthCheckInput{
			TenantID: params.TenantID, ResourceID: candidateCustomer(po.Candidates),
			Permission: authz.PermView, ResourceType: authz.ResCustomer, SubjectID: params.SenderID,
		}).Get(ctx, &authorized); err != nil {
			return err
		}
		if !authorized {
			if err := setPreState(ctx, params, domain.StatePreIntentParsing, domain.StatePreIgnored,
				"SenderUnauthorized", "消息发送者无客户权限（忽略+审计）", po.Candidates, params.SenderID); err != nil {
				return err
			}
			return nil
		}
	}
	if ires.HighConfidence {
		if err := setPreState(ctx, params, domain.StatePreIntentParsing, domain.StatePrePendingConfirm,
			"IntentParsed", "高置信且校验通过", po.Candidates, ""); err != nil {
			return err
		}
		// 推送确认卡片
		if err := act(ctx, ActNotifyPreOrder, params.TenantID, params.PreOrderID, po.Candidates).Get(ctx, nil); err != nil {
			return err
		}
		return preConfirmStep(ctx, params, po)
	}
	// 低置信/多候选/高金额 → AMBIGUOUS（轻提醒，不自动建单——AI 安全红线）
	if err := setPreState(ctx, params, domain.StatePreIntentParsing, domain.StatePreAmbiguous,
		"IntentAmbiguous", "低置信/多候选: "+ires.Reason, po.Candidates, ""); err != nil {
		return err
	}
	return preConfirmStep(ctx, params, po)
}

// preConfirmStep 人工确认闸门（Signal；24h Timer 升级）。
func preConfirmStep(ctx workflow.Context, params PreOrderWorkflowParams, po *repo.PreOrder) error {
	// po 为入口快照：重读当前状态（AMBIGUOUS 重入等）
	cur, err := getPreOrder(ctx, params)
	if err != nil {
		return err
	}
	po = cur
	from := po.State
	if from != domain.StatePrePendingConfirm && from != domain.StatePreAmbiguous && from != domain.StatePreEscalated {
		from = domain.StatePrePendingConfirm
	}
	chanConfirm := workflow.GetSignalChannel(ctx, SignalPreOrderConfirm)
	chanIgnore := workflow.GetSignalChannel(ctx, SignalPreOrderIgnore)
	type gate struct {
		kind string
		sig  PreOrderConfirmSignal
	}
	for {
		sel := workflow.NewSelector(ctx)
		g := &gate{}
		sel.AddReceive(chanConfirm, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, &g.sig)
			if g.sig.Confirmed {
				g.kind = "confirm"
			} else {
				g.kind = "modified"
			}
		})
		sel.AddReceive(chanIgnore, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, nil)
			g.kind = "ignore"
		})
		if params.EscalateAfter > 0 && !params.Shadow {
			// 影子模式：Timer 不调度（推进只认信号）
			timer := workflow.NewTimer(ctx, params.EscalateAfter)
			sel.AddFuture(timer, func(f workflow.Future) { _ = f.Get(ctx, nil); g.kind = "escalate" })
		}
		sel.Select(ctx)
		// 确认/修改前归一化到 PENDING_CONFIRM
		// （ESCALATED → PENDING_CONFIRM；AMBIGUOUS → PENDING_CONFIRM，设计 §2.6）
		if (from == domain.StatePreEscalated || from == domain.StatePreAmbiguous) &&
			(g.kind == "confirm" || g.kind == "modified") {
			if err := setPreState(ctx, params, from, domain.StatePrePendingConfirm,
				"PreOrderDeescalated", "轻提醒后继续等待确认", nil, "system"); err != nil {
				return err
			}
			from = domain.StatePrePendingConfirm
		}
		switch g.kind {
		case "confirm":
			if err := act(ctx, ActValidatePreDraft, params.TenantID, params.PreOrderID, g.sig.DraftVersion).Get(ctx, nil); err != nil {
				return err
			}
			if g.sig.DraftVersion > 0 {
				if err := act(ctx, ActRecordPreDecision, repo.PreOrderDecision{TenantID: params.TenantID, PreOrderID: params.PreOrderID, DraftVersion: g.sig.DraftVersion, Action: "CONFIRM", OperatorID: g.sig.Operator, BeforeData: po.Candidates, AfterData: po.Candidates}).Get(ctx, nil); err != nil {
					return err
				}
			}
			// 确认（未修改）→ CONFIRMED → 派生正式订单
			if err := setPreState(ctx, params, from, domain.StatePreConfirmed,
				"PreOrderConfirmed", "销售人工确认", po.Candidates, g.sig.Operator); err != nil {
				return err
			}
			return deriveOrder(ctx, params, po, false, 0, 0, g.sig.DraftVersion, g.sig.Operator)
		case "modified":
			version := g.sig.DraftVersion
			if err := act(ctx, ActValidatePreDraft, params.TenantID, params.PreOrderID, g.sig.DraftVersion).Get(ctx, nil); err != nil {
				return err
			}
			if g.sig.DraftVersion > 0 {
				if err := act(ctx, ActRecordPreDecision, repo.PreOrderDecision{TenantID: params.TenantID, PreOrderID: params.PreOrderID, DraftVersion: g.sig.DraftVersion, Action: "MODIFY_CONFIRM", OperatorID: g.sig.Operator, BeforeData: po.Candidates, AfterData: repo.MarshalJSONDetail(map[string]any{"quantity": g.sig.ModifiedQuantity, "unit_price_cents": g.sig.ModifiedUnitPriceCents})}).Get(ctx, nil); err != nil {
					return err
				}
				var draft *repo.PreOrderDraft
				if err := act(ctx, ActCreatePreDraft, repo.NewPreOrderDraftVersion{
					TenantID: params.TenantID, PreOrderID: params.PreOrderID, SourceMsgID: po.MsgID,
					SourceMsgHash: repo.HashPayload(po.Candidates), Candidate: po.Candidates,
					PolicyStatus: "NEEDS_CONFIRMATION", Evidence: []byte("[]"), Quote: []byte("{}"),
					Diff: repo.MarshalJSONDetail(map[string]any{"quantity": g.sig.ModifiedQuantity, "unit_price_cents": g.sig.ModifiedUnitPriceCents}),
				}).Get(ctx, &draft); err != nil {
					return err
				} else if draft != nil {
					version = draft.DraftVersion
				}
			}
			// 修改后确认 → MODIFIED
			if err := setPreState(ctx, params, from, domain.StatePreModified,
				"PreOrderModified", "销售修改后确认", po.Candidates, g.sig.Operator); err != nil {
				return err
			}
			return deriveOrder(ctx, params, po, true, g.sig.ModifiedQuantity, g.sig.ModifiedUnitPriceCents, version, g.sig.Operator)
		case "ignore":
			return setPreState(ctx, params, from, domain.StatePreIgnored,
				"PreOrderIgnored", "销售忽略", nil, g.sig.Operator)
		case "escalate":
			// 首次升级：PENDING_CONFIRM/AMBIGUOUS → ESCALATED；重复升级（Timer 再次触发）
			// 不再迁移状态（修正：原实现第二次触发时 ESCALATED → ESCALATED 自迁移被 FSM 拒绝）
			if from != domain.StatePreEscalated {
				if err := setPreState(ctx, params, from, domain.StatePreEscalated,
					"PreOrderEscalated", "超期未处理 Timer 升级", nil, "system"); err != nil {
					return err
				}
				from = domain.StatePreEscalated
			}
			// 升级后继续等待确认（重新推送卡片由通知方处理）
			_ = act(ctx, ActNotifyPreOrder, params.TenantID, params.PreOrderID, po.Candidates).Get(ctx, nil)
		}
	}
}

// deriveOrder 派生正式订单（订单幂等键 = pre:<tenant>:<group>:<msgid>:<intent_version> 的 order 变体）。
func deriveOrder(ctx workflow.Context, params PreOrderWorkflowParams, po *repo.PreOrder, modified bool, modQty, modPrice, draftVersion int64, operator string) error {
	// 订单幂等键：以预订单幂等键派生（source=pre）
	orderKey := "order:" + po.TenantID + ":pre:" + shortID(po.IdempotencyKey)
	var cands []IntentCandidate
	_ = jsonUnmarshal(po.Candidates, &cands)
	if len(cands) == 0 {
		return fmt.Errorf("preorder %s has no candidates", params.PreOrderID)
	}
	qty := cands[0].Quantity
	price := cands[0].UnitPriceCents
	if modified {
		if modQty > 0 {
			qty = modQty
		}
		if modPrice >= 0 {
			price = modPrice
		}
	}
	// 确定性校验（AI 安全边界：修改值同样过校验）
	if _, err := domain.NewQty(qty); err != nil {
		return err
	}
	if _, err := domain.NewAmountCents(price); err != nil {
		return err
	}
	customerID, paymentTerm, supplierChannel, warehouseID, receiveAddress := candidateOrderFields(po.Candidates)
	if customerID == "" || paymentTerm == "" || supplierChannel == "" || warehouseID == "" {
		if draftVersion > 0 {
			return fmt.Errorf("preorder %s candidate missing customer/payment/channel/warehouse master data", params.PreOrderID)
		}
		// 旧 API 预订单兼容路径：没有版本化 Agent 草稿时仍支持历史人工测试数据。
		// 真实 Agent 草稿必须携带主数据字段并走上面的严格分支。
		if customerID == "" {
			customerID = "cust-0001"
		}
		if paymentTerm == "" {
			paymentTerm = "款到"
		}
		if supplierChannel == "" {
			supplierChannel = "渠道A"
		}
		if warehouseID == "" {
			warehouseID = "wh-yangyuan"
		}
	}
	payload := repo.MarshalJSONDetail(map[string]any{
		"pre_order_id":     po.ID,
		"draft_version":    draftVersion,
		"group_id":         po.GroupID,
		"msg_id":           po.MsgID,
		"product_id":       cands[0].ProductID,
		"quantity":         qty,
		"unit_price_cents": price,
	})
	var ord *DeriveOrderResult
	if err := act(ctx, ActDeriveOrder, repo.NewOrder{
		TenantID: po.TenantID, CustomerID: customerID, PaymentTerm: paymentTerm,
		SupplierChannel: supplierChannel, IdempotencyKey: orderKey, Payload: payload,
		AgentTrace: &repo.OrderCreationTrace{PreOrderID: po.ID, DraftVersion: draftVersion, ConfirmedBy: operator, CorrelationID: po.TenantID + ":" + po.GroupID + ":" + po.MsgID},
		Items: []repo.NewOrderItem{{
			ID: "item-" + shortID(orderKey), ProductID: cands[0].ProductID,
			WarehouseID: strPtr(warehouseID), Quantity: qty, UnitPriceCents: price, ReceiveAddress: receiveAddress,
		}},
	}).Get(ctx, &ord); err != nil {
		return err
	}
	fromState := domain.StatePreConfirmed
	if modified {
		// 修改后确认链：preConfirmStep 已将 DB 状态置为 PRE_MODIFIED
		fromState = domain.StatePreModified
	}
	return setPreState(ctx, params, fromState, domain.StatePreOrderRequested,
		"OrderRequested", "预订单确认派生正式订单", payload, operator)
}

func candidateOrderFields(candidates []byte) (customerID, paymentTerm, supplierChannel, warehouseID, receiveAddress string) {
	var cands []IntentCandidate
	_ = jsonUnmarshal(candidates, &cands)
	if len(cands) > 0 && cands[0].CustomerID != "" {
		customerID = cands[0].CustomerID
		paymentTerm = cands[0].PaymentTerm
		supplierChannel = cands[0].SupplierChannel
		warehouseID = cands[0].WarehouseID
		receiveAddress = cands[0].ReceiveAddress
	}
	return
}

// candidateCustomer 取候选携带的客户 ID（0.1 A7/N-04：群上下文归属锁定）；
// 无候选或未带出客户时返回空串，授权校验将按空资源处理。
func candidateCustomer(candidates []byte) string {
	var cands []IntentCandidate
	_ = jsonUnmarshal(candidates, &cands)
	if len(cands) == 0 {
		return ""
	}
	return cands[0].CustomerID
}

// getPreOrder 读预订单（Activity）。
func getPreOrder(ctx workflow.Context, params PreOrderWorkflowParams) (*repo.PreOrder, error) {
	var po *repo.PreOrder
	err := act(ctx, ActGetPreOrder, params.TenantID, params.PreOrderID).Get(ctx, &po)
	return po, err
}

// setPreState 预订单状态迁移（FSM 校验 + Activity）。
func setPreState(ctx workflow.Context, params PreOrderWorkflowParams, from, to, action, reason string, detail []byte, operator string) error {
	if err := domain.PreOrderFSM.Validate(from, to); err != nil {
		return err
	}
	return act(ctx, ActUpdatePreOrder, params.TenantID, params.PreOrderID,
		from, to, action, reason, detail, operator).Get(ctx, nil)
}

func strPtr(s string) *string { return &s }
