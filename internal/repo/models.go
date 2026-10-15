package repo

import (
	"encoding/json"
	"time"
)

// ── 领域模型（与 migrations 表一一对应，docs/02-architecture.html §3.1）──

// Order 订单主档。
type Order struct {
	ID                  string
	TenantID            string
	CustomerID          string
	State               string
	PaymentTerm         string
	SupplierChannel     string
	ContractNo          *string
	IdempotencyKey      string
	PayloadHash         string
	Payload             []byte
	LegacyFlowID        *string
	SubmissionMode      string
	ResubmitFromOrderID *string
	ResubmitRootOrderID *string
	ResubmitRevision    int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OrderItem 订单明细（数量守恒基准）。
type OrderItem struct {
	ID             string
	TenantID       string
	OrderID        string
	ProductID      string
	WarehouseID    *string
	Quantity       int64
	UnitPriceCents int64
	// ReceiveAddress 收货地址（A1 新增：商务备货按收货地址选仓）。
	ReceiveAddress string
}

// StockPrepare 备货审批聚合。
type StockPrepare struct {
	ID                     string
	TenantID               string
	OrderID                string
	WarehouseID            string
	State                  string
	PlannedQty             int64
	RemainingQty           int64
	BatchSeq               int
	SplitFlag              bool
	PreviousStockPrepareID *string
	OutboundOrderRef       *string
	IdempotencyKey         string
	CreatedAt              time.Time
}

// StockPrepareItem 备货明细。
type StockPrepareItem struct {
	ID             string
	TenantID       string
	StockPrepareID string
	OrderItemID    string
	ProductID      string
	Qty            int64
}

// Shipment 发货单（OPEN/SHIPPED/CLOSED）。
type Shipment struct {
	ID                 string
	TenantID           string
	OrderID            string
	StockPrepareID     string
	WarehouseID        string
	State              string
	PlannedQty         int64
	ShippedQty         int64
	BatchSeq           int
	SplitFlag          bool
	PreviousShipmentID *string
	RemainingQuantity  int64
	IdempotencyKey     string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ShipmentItem 发货明细。
type ShipmentItem struct {
	ID          string
	TenantID    string
	ShipmentID  string
	OrderItemID string
	ProductID   string
	Qty         int64
}

// Invoice 开票申请/发票。
type Invoice struct {
	ID             string
	TenantID       string
	OrderID        string
	State          string
	AmountCents    int64
	Qty            int64
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// 三金额分展（A4 新增：收款/发货/开票；快照值，权威口径走 OrderThreeAmounts 查询）
	ReceivedCents int64
	ShippedCents  int64
	InvoicedCents int64
}

// InvoiceItem 开票明细（守恒校验基准）。
type InvoiceItem struct {
	ID             string
	TenantID       string
	InvoiceID      string
	OrderItemID    string
	ProductID      string
	Qty            int64
	AmountCents    int64
	IdempotencyKey string
}

// InvoiceOrderLink 发票-订单关联（A4：一笔款跨多笔订单；关联可追加）。
type InvoiceOrderLink struct {
	TenantID    string    `json:"tenant_id"`
	InvoiceID   string    `json:"invoice_id"`
	OrderID     string    `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Qty         int64     `json:"qty"`
	LinkedAt    time.Time `json:"linked_at"`
}

// OrderThreeAmounts 订单三金额分展（A4：收款/发货/开票分别展示）。
type OrderThreeAmounts struct {
	OrderID         string `json:"order_id"`
	ReceivedCents   int64  `json:"received_cents"`
	ShippedCents    int64  `json:"shipped_cents"`
	InvoicedCents   int64  `json:"invoiced_cents"`
	UninvoicedCents int64  `json:"uninvoiced_cents"`
}

// PreOrder 预订单（GET/POST /api/v1/preorders 直接序列化；契约：snake_case）。
type PreOrder struct {
	ID                    string    `json:"id"`
	TenantID              string    `json:"tenant_id"`
	GroupID               string    `json:"group_id"`
	MsgID                 string    `json:"msg_id"`
	ConversationID        string    `json:"conversation_id,omitempty"`
	RootMsgID             string    `json:"root_msg_id,omitempty"`
	ConfirmedDraftVersion *int64    `json:"confirmed_draft_version,omitempty"`
	State                 string    `json:"state"`
	Candidates            []byte    `json:"candidates"`
	IdempotencyKey        string    `json:"idempotency_key"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// PreOrderDraft 不可变的预订单草稿版本。
type PreOrderDraft struct {
	TenantID      string          `json:"tenant_id"`
	PreOrderID    string          `json:"pre_order_id"`
	DraftVersion  int64           `json:"draft_version"`
	SourceMsgID   string          `json:"source_msg_id"`
	SourceMsgHash string          `json:"source_msg_hash"`
	Quote         json.RawMessage `json:"quote"`
	Evidence      json.RawMessage `json:"evidence"`
	Candidate     json.RawMessage `json:"candidate"`
	PolicyStatus  string          `json:"policy_status"`
	PolicyResult  json.RawMessage `json:"policy_result"`
	CustomerBasis json.RawMessage `json:"customer_basis"`
	ProductBasis  json.RawMessage `json:"product_basis"`
	ContractBasis json.RawMessage `json:"contract_basis"`
	PriceBasis    json.RawMessage `json:"price_basis"`
	ModelName     string          `json:"model_name"`
	ModelVersion  string          `json:"model_version"`
	PromptVersion string          `json:"prompt_version"`
	PolicyVersion string          `json:"policy_version"`
	Status        string          `json:"status"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	Diff          json.RawMessage `json:"diff"`
	SnapshotJSON  json.RawMessage `json:"snapshot_json"`
	PatchJSON     json.RawMessage `json:"patch_json"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

// InboundMessage 是平台消息事实，负责消息级幂等和处理生命周期。
type InboundMessage struct {
	ID            int64     `json:"id"`
	TenantID      string    `json:"tenant_id"`
	GroupID       string    `json:"group_id"`
	MsgID         string    `json:"msg_id"`
	SenderID      string    `json:"sender_id"`
	MessageSeq    *int64    `json:"message_seq,omitempty"`
	ReplyToMsgID  string    `json:"reply_to_msg_id,omitempty"`
	RawText       string    `json:"raw_text,omitempty"`
	TextHash      string    `json:"text_hash"`
	ReceivedAt    time.Time `json:"received_at"`
	ParseStatus   string    `json:"parse_status"`
	RetryCount    int       `json:"retry_count"`
	SessionID     string    `json:"session_id,omitempty"`
	DraftVersion  *int64    `json:"draft_version,omitempty"`
	CorrelationID string    `json:"correlation_id"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// OrderConversation 是多轮订单归并的事实载体，一个会话对应一个主预订单。
type OrderConversation struct {
	SessionID             string     `json:"session_id"`
	TenantID              string     `json:"tenant_id"`
	GroupID               string     `json:"group_id"`
	SenderID              string     `json:"sender_id"`
	CustomerID            *string    `json:"customer_id,omitempty"`
	OwnerSalesID          *string    `json:"owner_sales_id,omitempty"`
	PreOrderID            string     `json:"pre_order_id"`
	CurrentDraftVersion   int64      `json:"current_draft_version"`
	LastDraftVersion      int64      `json:"last_draft_version"`
	ConfirmedDraftVersion *int64     `json:"confirmed_draft_version,omitempty"`
	Status                string     `json:"status"`
	LastMessageAt         time.Time  `json:"last_message_at"`
	EscalateAt            *time.Time `json:"escalate_at,omitempty"`
	ExpiresAt             time.Time  `json:"expires_at"`
	CorrelationID         string     `json:"correlation_id"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// PreOrderDraftSource 记录一个草稿版本吸收的每条消息。
type PreOrderDraftSource struct {
	TenantID     string    `json:"tenant_id"`
	PreOrderID   string    `json:"pre_order_id"`
	DraftVersion int64     `json:"draft_version"`
	MessageID    int64     `json:"message_id"`
	SourceRole   string    `json:"source_role"`
	Quote        string    `json:"quote"`
	CreatedAt    time.Time `json:"created_at"`
}

// PreOrderDecision 预订单人工决策审计记录。
type PreOrderDecision struct {
	ID           int64           `json:"id"`
	TenantID     string          `json:"tenant_id"`
	PreOrderID   string          `json:"pre_order_id"`
	DraftVersion int64           `json:"draft_version"`
	Action       string          `json:"action"`
	OperatorID   string          `json:"operator_id"`
	Reason       string          `json:"reason"`
	BeforeData   json.RawMessage `json:"before_data"`
	AfterData    json.RawMessage `json:"after_data"`
	CreatedAt    time.Time       `json:"created_at"`
}

// PolicyEvaluation 确定性策略执行快照。
type PolicyEvaluation struct {
	ID            int64           `json:"id"`
	TenantID      string          `json:"tenant_id"`
	PreOrderID    string          `json:"pre_order_id"`
	DraftVersion  int64           `json:"draft_version"`
	PolicyVersion string          `json:"policy_version"`
	InputSnapshot json.RawMessage `json:"input_snapshot"`
	Output        json.RawMessage `json:"output"`
	RuleHits      json.RawMessage `json:"rule_hits"`
	Status        string          `json:"status"`
	CorrelationID string          `json:"correlation_id"`
	CreatedAt     time.Time       `json:"created_at"`
}

// WorkflowOutboxEvent 工作流启动 outbox（独立于 SpiceDB outbox_events）。
type WorkflowOutboxEvent struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	EventKey    string          `json:"event_key"`
	EventType   string          `json:"event_type"`
	AggregateID string          `json:"aggregate_id"`
	WorkflowID  string          `json:"workflow_id"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	AvailableAt time.Time       `json:"available_at"`
	LockedUntil *time.Time      `json:"locked_until,omitempty"`
	LastError   string          `json:"last_error"`
	CreatedAt   time.Time       `json:"created_at"`
	ProcessedAt *time.Time      `json:"processed_at,omitempty"`
}

// DeadLetterEvent 无法自动恢复的事件。
type DeadLetterEvent struct {
	ID         int64           `json:"id"`
	TenantID   string          `json:"tenant_id"`
	Source     string          `json:"source"`
	SourceID   string          `json:"source_id"`
	Payload    json.RawMessage `json:"payload"`
	Error      string          `json:"error"`
	Attempts   int             `json:"attempts"`
	Resolved   bool            `json:"resolved"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy *string         `json:"resolved_by,omitempty"`
}

// AgentOrderTrace 汇总消息、草稿、确认、订单和工作流 outbox 关联。
type AgentOrderTrace struct {
	TenantID      string                `json:"tenant_id"`
	GroupID       string                `json:"group_id"`
	MsgID         string                `json:"msg_id"`
	PreOrderID    string                `json:"pre_order_id"`
	DraftVersion  int64                 `json:"draft_version"`
	OrderID       string                `json:"order_id"`
	WorkflowID    string                `json:"workflow_id"`
	Confirmations []PreOrderDecision    `json:"confirmations"`
	Outbox        []WorkflowOutboxEvent `json:"outbox"`
	DeadLetters   []DeadLetterEvent     `json:"dead_letters"`
}

// ApprovalTask 审批任务（GET /api/v1/todos 直接序列化；契约：snake_case）。
type ApprovalTask struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	OrderID      string     `json:"order_id"`
	StageCode    string     `json:"stage_code"`
	Kind         string     `json:"kind"`
	State        string     `json:"state"`
	AssigneeID   *string    `json:"assignee_id,omitempty"`
	LegacyTaskID *string    `json:"legacy_task_id,omitempty"`
	Deadline     *time.Time `json:"deadline,omitempty"`
	Verdict      *string    `json:"verdict,omitempty"`
	DecidedBy    *string    `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Event 分域事件（审计回放，CQ-12）。
type Event struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Aggregate   string          `json:"aggregate"` // orders / stock_prepares / shipments / invoices / pre_orders
	AggregateID string          `json:"aggregate_id"`
	OrderID     string          `json:"order_id"`
	StageCode   string          `json:"stage_code"`
	TaskID      *string         `json:"task_id,omitempty"`
	Action      string          `json:"action"`
	FromState   *string         `json:"from_state,omitempty"`
	ToState     *string         `json:"to_state,omitempty"`
	Reason      *string         `json:"reason,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	Error       *string         `json:"error,omitempty"`
	OperatorID  *string         `json:"operator_id,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// InventoryItem 库存（仓库 × 产品）。
type InventoryItem struct {
	TenantID    string
	WarehouseID string
	ProductID   string
	Qty         int64
	Version     int64
}

// ReconcileOrder 单订单守恒聚合结果（L3-02 对账脚本数据源）。
type ReconcileOrder struct {
	TenantID       string
	OrderID        string
	OrderQty       int64
	PreparedQty    int64
	ShippedQty     int64
	InvoicedQty    int64
	InvoicedAmount int64
	Violations     []string
}
