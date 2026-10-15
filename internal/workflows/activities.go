package workflows

import (
	"context"
	"encoding/json"
	"fmt"

	"b2b_pilot_v3/internal/agentorder"
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
)

// ── Activity 池（docs/02-architecture.html §2.2）──
// 全部副作用（PG/SpiceDB/企微）只发生在 Activity；Workflow 仅做确定性编排。

// Activity 名称常量（worker/test env 按名注册）。
const (
	ActGetOrder            = "GetOrder"
	ActGetOrderItems       = "GetOrderItems"
	ActGetCustomerGrade    = "GetCustomerGrade"
	ActGetCustomer         = "GetCustomer"
	ActValidateOrderData   = "ValidateOrderData"
	ActValidateOrder       = "ValidateOrder"
	ActGenerateContract    = "GenerateContract"
	ActAuthCheck           = "AuthCheck"
	ActCreateApprovalTask  = "CreateApprovalTask"
	ActDecideTask          = "DecideTask"
	ActReserveStock        = "ReserveStock"
	ActShipGoods           = "ShipGoods"
	ActApplyInvoice        = "ApplyInvoice"
	ActTrackPayment        = "TrackPayment"
	ActArchiveOrder        = "ArchiveOrder"
	ActCompensate          = "Compensate"
	ActNotifyApproval      = "NotifyApproval"
	ActNotifyPreOrder      = "NotifyPreOrderConfirm"
	ActNotifyOrderEvent    = "NotifyOrderEvent"
	ActCreatePreOrder      = "CreatePreOrder"
	ActParseIntent         = "ParseIntent"
	ActDeriveOrder         = "DeriveOrder"
	ActGetPreOrder         = "GetPreOrder"
	ActValidatePreDraft    = "ValidatePreOrderDraft"
	ActEvaluateAgentPolicy = "EvaluateAgentPolicy"
	ActRecordPreDecision   = "RecordPreOrderDecision"
	ActCreatePreDraft      = "CreatePreOrderDraftVersion"
	ActUpdatePreOrder      = "UpdatePreOrderState"
	ActUpdateResubmit      = "UpdateResubmitChain"
	ActUpdateMeta          = "UpdateOrderMeta"
	ActGetPendingTask      = "GetPendingTask"
	ActCountShipments      = "CountShipments"
	ActGetStockPrepare     = "GetStockPrepare"
)

// Activities 活动实现（依赖经构造注入；L2 用 fakeStore/fakeNotifier/StaticChecker）。
type Activities struct {
	Store    Store
	Data     orderdata.Provider
	Notifier Notifier
	Authz    AuthChecker
	// ContractGate 合同生成故障钩子（测试注入：模拟要素不全 → 补全重校验分支）
	ContractGate func(in GenerateContractInput) (*GenerateContractResult, error)
}

type policyEvaluationStore interface {
	RecordPolicyEvaluation(context.Context, repo.PolicyEvaluation) error
}

// EvaluateAgentPolicy 执行确定性策略并保存完整输入/输出快照。
func (a *Activities) EvaluateAgentPolicy(ctx context.Context, in agentorder.PolicyInput, preOrderID string, draftVersion int64) (*agentorder.PolicyDecision, error) {
	decision := agentorder.EvaluatePolicy(in)
	if err := decision.Validate(); err != nil {
		return nil, err
	}
	if s, ok := a.Store.(policyEvaluationStore); ok {
		out, _ := json.Marshal(decision.Output)
		hits, _ := json.Marshal(decision.RuleHits)
		if err := s.RecordPolicyEvaluation(ctx, repo.PolicyEvaluation{
			TenantID: in.TenantID, PreOrderID: preOrderID, DraftVersion: draftVersion,
			PolicyVersion: decision.PolicyVersion, InputSnapshot: decision.InputSnapshot,
			Output: out, RuleHits: hits, Status: string(decision.Status), CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
	}
	return &decision, nil
}

// ── 授权独立验证者（L2-14：每个业务副作用前重复验证）──

// AuthCheckInput 授权检查输入。
type AuthCheckInput struct {
	TenantID     string
	ResourceID   string
	Permission   authz.Permission
	SubjectID    string
	ResourceType authz.ResourceType
}

// AuthCheck 授权判定 Activity（FullyConsistent；纵深防御第一道）。
func (a *Activities) AuthCheck(ctx context.Context, in AuthCheckInput) (bool, error) {
	return a.Authz.Check(ctx, authz.CheckRequest{
		Permission:   in.Permission,
		ResourceType: in.ResourceType,
		ResourceID:   in.ResourceID,
		SubjectID:    in.SubjectID,
	})
}

// ── 订单查询/校验 ──

// GetOrder 读订单。
func (a *Activities) GetOrder(ctx context.Context, tenantID, orderID string) (*repo.Order, error) {
	return a.Store.GetOrder(ctx, tenantID, orderID)
}

// GetOrderItems 读明细。
func (a *Activities) GetOrderItems(ctx context.Context, tenantID, orderID string) ([]repo.OrderItem, error) {
	return a.Store.GetOrderItems(ctx, tenantID, orderID)
}

// GetCustomerGrade 读客户等级（C/D 触发董事长审批）。
// 生产路径经 orderdata.Provider（pgAPI）；Data 为空时兼容旧测试替身（FakeStore 提供同签名方法）。
func (a *Activities) GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error) {
	if a.Data != nil {
		return a.Data.GetCustomerGrade(ctx, tenantID, customerID)
	}
	if s, ok := a.Store.(customerGradeStore); ok {
		return s.GetCustomerGrade(ctx, tenantID, customerID)
	}
	return "", fmt.Errorf("order data provider not configured (customer %s)", customerID)
}

type customerGradeStore interface {
	GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error)
}

// GetCustomer 读客户数据实体（合同生成需要客户名）。
// Data 为空时返回历史占位客户，保持旧测试行为不变。
func (a *Activities) GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error) {
	if a.Data != nil {
		return a.Data.GetCustomer(ctx, tenantID, customerID)
	}
	grade := ""
	if s, ok := a.Store.(customerGradeStore); ok {
		g, err := s.GetCustomerGrade(ctx, tenantID, customerID)
		if err != nil {
			return nil, err
		}
		grade = g
	}
	return &pgapi.Customer{RecordID: customerID, CustomerName: "客户", CustomerLevel: grade}, nil
}

// ValidateOrderDataResult 订单数据实体校验结果（客户 + 产品）。
type ValidateOrderDataResult struct {
	CustomerName  string
	CustomerGrade string
}

// ValidateOrderData 订单数据实体校验：客户存在、等级合法由 ValidateOrder 复用；
// 产品 record_id 必须在 pgAPI 产品池存在（替代已移除的本地 products FK）。
func (a *Activities) ValidateOrderData(ctx context.Context, tenantID, customerID string, items []repo.OrderItem) (*ValidateOrderDataResult, error) {
	if a.Data == nil {
		// 旧测试替身兼容：FakeStore 的客户等级即客户存在性；产品校验跳过。
		grade := ""
		if s, ok := a.Store.(customerGradeStore); ok {
			g, err := s.GetCustomerGrade(ctx, tenantID, customerID)
			if err != nil {
				return nil, err
			}
			grade = g
		}
		return &ValidateOrderDataResult{CustomerName: "客户", CustomerGrade: grade}, nil
	}
	customer, err := a.Data.GetCustomer(ctx, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("pgapi customer %s: %w", customerID, err)
	}
	seen := map[string]bool{}
	for _, it := range items {
		if it.ProductID == "" {
			return nil, fmt.Errorf("order item %s has empty product_id", it.ID)
		}
		if seen[it.ProductID] {
			continue
		}
		seen[it.ProductID] = true
		if _, err := a.Data.GetProduct(ctx, tenantID, it.ProductID); err != nil {
			return nil, fmt.Errorf("pgapi product %s: %w", it.ProductID, err)
		}
	}
	return &ValidateOrderDataResult{CustomerName: customer.CustomerName, CustomerGrade: customer.CustomerLevel}, nil
}

// ValidateOrderResult 校验结果。
type ValidateOrderResult struct {
	OK            bool
	Reason        string
	CustomerGrade string
	PaymentTerm   string
}

// ValidateOrder 确定性校验（AI 安全边界：LLM 只产候选，这里做最终裁决）：
// 明细数量>0、单价>=0、客户存在且等级合法、幂等键合法。
func (a *Activities) ValidateOrder(ctx context.Context, ord *repo.Order, items []repo.OrderItem, grade string) (*ValidateOrderResult, error) {
	res := &ValidateOrderResult{CustomerGrade: grade, PaymentTerm: ord.PaymentTerm}
	if _, _, _, err := domain.ParseOrderKey(ord.IdempotencyKey); err != nil {
		res.Reason = "非法幂等键: " + err.Error()
		return res, nil
	}
	if grade != "A" && grade != "B" && grade != "C" && grade != "D" {
		res.Reason = "客户等级非法: " + grade
		return res, nil
	}
	if len(items) == 0 {
		res.Reason = "订单无明细"
		return res, nil
	}
	for _, it := range items {
		if _, err := domain.NewQty(it.Quantity); err != nil {
			res.Reason = fmt.Sprintf("明细数量非法: %s", it.ID)
			return res, nil
		}
		if _, err := domain.NewAmountCents(it.UnitPriceCents); err != nil {
			res.Reason = fmt.Sprintf("明细单价非法: %s", it.ID)
			return res, nil
		}
	}
	res.OK = true
	return res, nil
}

// ── 合同生成（确定性）──

// GenerateContractInput 合同要素。
type GenerateContractInput struct {
	TenantID     string
	OrderID      string
	Order        *repo.Order
	Items        []repo.OrderItem
	CustomerName string
}

// GenerateContractResult 合同生成结果。
type GenerateContractResult struct {
	ContractNo string
	OK         bool
	Missing    []string // 缺失要素（补全重校验，L2-05）
}

// GenerateContract 合同生成校验：模板/要素/编号（基线为确定性规则；
// 要素不全 → OK=false + Missing 清单 → 补全后重校验）。
func (a *Activities) GenerateContract(ctx context.Context, in GenerateContractInput) (*GenerateContractResult, error) {
	if a.ContractGate != nil {
		return a.ContractGate(in)
	}
	res := &GenerateContractResult{}
	if in.CustomerName == "" {
		res.Missing = append(res.Missing, "customer_name")
	}
	if in.Order.PaymentTerm == "" {
		res.Missing = append(res.Missing, "payment_term")
	}
	if len(in.Items) == 0 {
		res.Missing = append(res.Missing, "items")
	}
	for _, it := range in.Items {
		if it.ProductID == "" {
			res.Missing = append(res.Missing, "product_id")
		}
	}
	if len(res.Missing) > 0 {
		return res, nil
	}
	// 合同号：HT-<tenant 短码>-<订单号后 8 位>
	short := in.OrderID
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	res.ContractNo = fmt.Sprintf("HT-%s-%s", in.TenantID, short)
	res.OK = true
	return res, nil
}

// ── 审批任务 ──

// CreateApprovalTask 创建审批待办 + 通知。
func (a *Activities) CreateApprovalTask(ctx context.Context, in repo.NewApprovalTask) (string, error) {
	id, err := a.Store.CreateApprovalTaskTx(ctx, in)
	if err != nil {
		return "", err
	}
	assignee := ""
	if in.AssigneeID != nil {
		assignee = *in.AssigneeID
	}
	if a.Notifier != nil {
		_ = a.Notifier.NotifyApproval(ctx, in.TenantID, in.OrderID, in.Kind, assignee, id)
	}
	return id, nil
}

// DecideTask 审批裁决（Signal → 待办状态落库）。
func (a *Activities) DecideTask(ctx context.Context, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error {
	return a.Store.DecideTaskTx(ctx, tenantID, taskID, toState, verdict, decidedBy, detail)
}

// ── 业务操作（单事务，见 Store 契约）──

// ReserveStock 商务备货：选仓+发货量 → 备货聚合 + 状态迁移（from/to 由调用方给定；
// 款到路径 STK_STOCK_REVIEWING→STK_WAREHOUSE_CONFIRM；非款到隐式备货直达发货分配）。
func (a *Activities) ReserveStock(ctx context.Context, sp repo.StockPrepare, items []repo.StockPrepareItem, fromState, toState, operator, reason string, detail []byte) error {
	return a.Store.ReserveStockTx(ctx, sp, items, fromState, toState, operator, reason, detail)
}

// ShipGoods 仓管实际发货（发货+库存扣减+状态迁移单事务；幂等键防重放）。
func (a *Activities) ShipGoods(ctx context.Context, ship repo.Shipment, items []repo.ShipmentItem, fromState, toState, operator, reason string, detail []byte) error {
	return a.Store.ShipAndUpdateOrderTx(ctx, ship, items, fromState, toState, operator, reason, detail)
}

// ApplyInvoice 财务确认开票（守恒校验+状态迁移单事务）。
func (a *Activities) ApplyInvoice(ctx context.Context, inv repo.Invoice, items []repo.InvoiceItem, fromState, toState, operator, reason string, detail []byte) error {
	return a.Store.InvoiceAndUpdateOrderTx(ctx, inv, items, fromState, toState, operator, reason, detail)
}

// SumShipped 聚合发货量（完成条件判定）。
func (a *Activities) SumShipped(ctx context.Context, tenantID, orderID string) (map[string]int64, error) {
	return a.Store.SumShippedByOrderItem(ctx, tenantID, orderID)
}

// GetStockPrepare 读备货单（0.1 选仓生效：发货单从备货单继承所选仓库）。
func (a *Activities) GetStockPrepare(ctx context.Context, tenantID, stockPrepareID string) (*repo.StockPrepare, error) {
	return a.Store.GetStockPrepare(ctx, tenantID, stockPrepareID)
}

// TrackPayment 回款登记（事件留痕 + 发票收款金额快照；金额由 Signal 携带）。
func (a *Activities) TrackPayment(ctx context.Context, tenantID, orderID, operator string, amount int64) error {
	if err := a.Store.TrackPaymentTx(ctx, tenantID, orderID, amount); err != nil {
		return err
	}
	return a.Store.UpdateOrderStateTx(ctx, tenantID, orderID, domain.StateINVInvoiced,
		domain.StateINVPaymentTracking, "PaymentTracked", operator,
		fmt.Sprintf("回款登记 ¥%d", amount), repo.MarshalJSONDetail(map[string]any{"amount": amount}))
}

// TrackPaymentFromNone 无需开票路径的回款登记。
func (a *Activities) TrackPaymentFromNone(ctx context.Context, tenantID, orderID, operator string, amount int64) error {
	return a.Store.UpdateOrderStateTx(ctx, tenantID, orderID, domain.StateINVNoInvoiceNeeded,
		domain.StateINVPaymentTracking, "PaymentTracked", operator,
		fmt.Sprintf("回款登记 ¥%d", amount), repo.MarshalJSONDetail(map[string]any{"amount": amount}))
}

// ArchiveOrder 合同归档 → 订单关闭（终态）。
func (a *Activities) ArchiveOrder(ctx context.Context, tenantID, orderID, operator string) error {
	return a.Store.UpdateOrderStateTx(ctx, tenantID, orderID, domain.StateINVContractArchived,
		domain.StateINVClosed, "OrderClosed", operator, "合同归档完成", nil)
}

// Compensate 补偿（基线：事件留痕 + 通知；真实补偿动作留后续）。
func (a *Activities) Compensate(ctx context.Context, tenantID, orderID, reason string, detail []byte) error {
	return a.Store.RecordFlowFailed(ctx, tenantID, orderID, reason, detail)
}

// ── 通知 ──

// NotifyApproval 审批通知。
func (a *Activities) NotifyApproval(ctx context.Context, tenantID, orderID, kind, assignee, taskID string) error {
	if a.Notifier == nil {
		return nil
	}
	return a.Notifier.NotifyApproval(ctx, tenantID, orderID, kind, assignee, taskID)
}

// NotifyPreOrderConfirm 预订单确认卡片。
func (a *Activities) NotifyPreOrderConfirm(ctx context.Context, tenantID, preOrderID string, candidates []byte) error {
	if a.Notifier == nil {
		return nil
	}
	return a.Notifier.NotifyPreOrderConfirm(ctx, tenantID, preOrderID, candidates)
}

// NotifyOrderEvent 状态通知。
func (a *Activities) NotifyOrderEvent(ctx context.Context, tenantID, orderID, state string) error {
	if a.Notifier == nil {
		return nil
	}
	return a.Notifier.NotifyOrderEvent(ctx, tenantID, orderID, state)
}

// ── 预订单（企微 S2；CQ-11）──

// CreatePreOrderResult 建预订单结果。
type CreatePreOrderResult struct {
	PreOrder *repo.PreOrder
	Created  bool
}

// CreatePreOrder 消息幂等建预订单。
func (a *Activities) CreatePreOrder(ctx context.Context, in repo.NewPreOrder) (*CreatePreOrderResult, error) {
	po, created, err := a.Store.CreatePreOrder(ctx, in)
	if err != nil {
		return nil, err
	}
	return &CreatePreOrderResult{PreOrder: po, Created: created}, nil
}

// GetPreOrder 读预订单。
func (a *Activities) GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*repo.PreOrder, error) {
	return a.Store.GetPreOrder(ctx, tenantID, preOrderID)
}

// ValidatePreOrderDraft 校验确认卡片版本没有落后于当前草稿。
func (a *Activities) ValidatePreOrderDraft(ctx context.Context, tenantID, preOrderID string, version int64) error {
	s, ok := a.Store.(interface {
		GetCurrentPreOrderDraft(context.Context, string, string) (*repo.PreOrderDraft, error)
	})
	if !ok {
		return nil // 旧测试替身没有版本化草稿存储。
	}
	draft, err := s.GetCurrentPreOrderDraft(ctx, tenantID, preOrderID)
	if err != nil {
		if version <= 0 && repo.IsNotFound(err) {
			return nil // 兼容历史人工预订单。
		}
		return err
	}
	if version <= 0 {
		return fmt.Errorf("draft_version_required: current=%d", draft.DraftVersion)
	}
	if draft.DraftVersion != version {
		return fmt.Errorf("draft_version_conflict: expected=%d actual=%d", version, draft.DraftVersion)
	}
	return nil
}

func (a *Activities) RecordPreOrderDecision(ctx context.Context, in repo.PreOrderDecision) error {
	if s, ok := a.Store.(interface {
		RecordPreOrderDecision(context.Context, repo.PreOrderDecision) (*repo.PreOrderDecision, error)
	}); ok {
		_, err := s.RecordPreOrderDecision(ctx, in)
		return err
	}
	return nil
}

func (a *Activities) CreatePreOrderDraftVersion(ctx context.Context, in repo.NewPreOrderDraftVersion) (*repo.PreOrderDraft, error) {
	s, ok := a.Store.(interface {
		CreatePreOrderDraftVersion(context.Context, repo.NewPreOrderDraftVersion) (*repo.PreOrderDraft, error)
	})
	if !ok {
		return nil, fmt.Errorf("draft store not configured")
	}
	return s.CreatePreOrderDraftVersion(ctx, in)
}

// UpdatePreOrderState 预订单状态迁移。
func (a *Activities) UpdatePreOrderState(ctx context.Context, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error {
	return a.Store.UpdatePreOrderStateTx(ctx, tenantID, preOrderID, fromState, toState, action, reason, detail, operator)
}

// IntentCandidate LLM 候选（AI 安全边界：只产候选，确定性校验裁决）。
type IntentCandidate struct {
	ProductSKU     string  `json:"product_sku"`
	ProductID      string  `json:"product_id"`
	Quantity       int64   `json:"quantity"`
	UnitPriceCents int64   `json:"unit_price_cents"`
	Confidence     float64 `json:"confidence"`
	// CustomerID 群上下文带出的客户（A7/N-04：归属锁定；确认派生订单时使用）
	CustomerID      string `json:"customer_id,omitempty"`
	PaymentTerm     string `json:"payment_term,omitempty"`
	SupplierChannel string `json:"supplier_channel,omitempty"`
	WarehouseID     string `json:"warehouse_id,omitempty"`
	ReceiveAddress  string `json:"receive_address,omitempty"`
}

// ParseIntentResult 意图解析结果（LLM 候选 + 确定性校验）。
type ParseIntentResult struct {
	Candidates     []IntentCandidate
	HighConfidence bool
	Reason         string
}

// ParseIntent 意图识别（基线：规则解析 mock——外部消息文本 → 候选；
// LLM 接线点保留在 wecom 层，见 internal/wecom。确定性校验：SKU 存在、数量>0、价格>=0）。
func (a *Activities) ParseIntent(ctx context.Context, tenantID string, candidates []byte) (*ParseIntentResult, error) {
	// 基线实现：候选已由 wecom 层解析（mock/LLM 输出统一 JSON 结构），此处做确定性校验
	var cands []IntentCandidate
	if err := jsonUnmarshal(candidates, &cands); err != nil || len(cands) == 0 {
		return &ParseIntentResult{Reason: "候选为空或格式非法"}, nil
	}
	high := true
	reason := ""
	for i := range cands {
		if cands[i].Quantity <= 0 || cands[i].UnitPriceCents < 0 {
			high = false
			reason = fmt.Sprintf("候选 %d 数量/价格非法", i)
		}
		if cands[i].Confidence < 0.8 {
			high = false
			reason = "低置信候选"
		}
	}
	return &ParseIntentResult{Candidates: cands, HighConfidence: high, Reason: reason}, nil
}

// DeriveOrderResult 派生订单结果。
type DeriveOrderResult struct {
	Order   *repo.Order
	Created bool
}

// DeriveOrder 预订单确认 → 派生正式订单（订单幂等键，双层防重；CQ-11）。
func (a *Activities) DeriveOrder(ctx context.Context, in repo.NewOrder) (*DeriveOrderResult, error) {
	if s, ok := a.Store.(interface {
		CreateOrderWithWorkflowOutbox(context.Context, repo.NewOrder, repo.OrderCreationTrace) (*repo.Order, bool, error)
	}); ok {
		trace := repo.OrderCreationTrace{}
		if in.AgentTrace != nil {
			trace = *in.AgentTrace
		}
		ord, created, err := s.CreateOrderWithWorkflowOutbox(ctx, in, trace)
		if err != nil {
			return nil, err
		}
		return &DeriveOrderResult{Order: ord, Created: created}, nil
	}
	ord, created, err := a.Store.CreateOrder(ctx, in)
	if err != nil {
		return nil, err
	}
	return &DeriveOrderResult{Order: ord, Created: created}, nil
}

// ── 辅助 ──

// jsonUnmarshal JSON 解析辅助。
func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

// NewActivities 构造活动集（兼容旧测试装配：Data 为空时走旧 Store 客户等级兼容路径）。
func NewActivities(store Store, notifier Notifier, checker AuthChecker) *Activities {
	return &Activities{Store: store, Notifier: notifier, Authz: checker}
}

// NewActivitiesWithData 构造活动集并注入订单数据实体 Provider（生产装配）。
func NewActivitiesWithData(store Store, notifier Notifier, checker AuthChecker, data orderdata.Provider) *Activities {
	return &Activities{Store: store, Data: data, Notifier: notifier, Authz: checker}
}
