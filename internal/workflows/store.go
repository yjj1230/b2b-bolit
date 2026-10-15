// Package workflows —— Temporal Workflow + Activity（docs/02-architecture.html §2.2）
//
// 编排纪律（Graph Engineering）：Workflow 只做确定性决策（状态迁移、Signal 等待、
// Timer 编排）；所有副作用（PG/SpiceDB/企微）都在 Activity，经 Store/Notifier 接口
// 注入——L2 用 fakeStore/fakeNotifier（纯库 TestSuite），生产用 repo.Repo + wecom.Gateway。
package workflows

import (
	"context"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
)

// Store 持久层契约（Activity 唯一数据入口；repo.Repo 结构实现，L2 用 fakeStore）。
// 每个方法一个原子事务（docs/02-architecture.html §4 防线 4）。
type Store interface {
	// 订单
	GetOrder(ctx context.Context, tenantID, orderID string) (*repo.Order, error)
	GetOrderItems(ctx context.Context, tenantID, orderID string) ([]repo.OrderItem, error)
	CreateOrder(ctx context.Context, in repo.NewOrder) (*repo.Order, bool, error)
	UpdateOrderStateTx(ctx context.Context, tenantID, orderID, fromState, toState, action, operator, reason string, detail []byte) error
	UpdateOrderMetaTx(ctx context.Context, tenantID, orderID, contractNo string, payload []byte) error
	SetOrderContractTx(ctx context.Context, tenantID, orderID, contractNo string) error
	UpdateResubmitChainTx(ctx context.Context, tenantID, orderID string, revision int, fromOrderID string) error
	// 备货/发货/开票（单事务业务操作）
	ReserveStockTx(ctx context.Context, sp repo.StockPrepare, items []repo.StockPrepareItem, fromState, toState, operator, reason string, detail []byte) error
	ShipAndUpdateOrderTx(ctx context.Context, ship repo.Shipment, items []repo.ShipmentItem, fromState, toState, operator, reason string, detail []byte) error
	InvoiceAndUpdateOrderTx(ctx context.Context, inv repo.Invoice, items []repo.InvoiceItem, fromState, toState, operator, reason string, detail []byte) error
	SumShippedByOrderItem(ctx context.Context, tenantID, orderID string) (map[string]int64, error)
	GetShipment(ctx context.Context, tenantID, shipmentID string) (*repo.Shipment, error)
	GetStockPrepare(ctx context.Context, tenantID, stockPrepareID string) (*repo.StockPrepare, error)
	// 凭证链（CQ-15）
	AppendEvidence(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string) (int, error)
	// 回款登记（A4：更新订单发票收款金额快照；权威口径为事件聚合）
	TrackPaymentTx(ctx context.Context, tenantID, orderID string, amount int64) error
	ListEvidenceVersions(ctx context.Context, tenantID, shipmentID string) ([]repo.EvidenceVersion, error)
	CountShipments(ctx context.Context, tenantID, orderID string) (int, error)
	// 审批任务
	CreateApprovalTaskTx(ctx context.Context, in repo.NewApprovalTask) (string, error)
	DecideTaskTx(ctx context.Context, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error
	GetApprovalTask(ctx context.Context, tenantID, taskID string) (*repo.ApprovalTask, error)
	ListApprovalTasks(ctx context.Context, tenantID string, assignee, kind, state string, limit int) ([]repo.ApprovalTask, error)
	// 查询（CQ-12 事件链 / 待办）
	ListOrderEvents(ctx context.Context, tenantID, orderID string, limit int) ([]repo.Event, error)
	ListPreOrderEvents(ctx context.Context, tenantID, preOrderID string) ([]repo.Event, error)
	// 预订单
	CreatePreOrder(ctx context.Context, in repo.NewPreOrder) (*repo.PreOrder, bool, error)
	GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*repo.PreOrder, error)
	UpdatePreOrderStateTx(ctx context.Context, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error
	// 补偿（事件留痕，不改状态）
	RecordFlowFailed(ctx context.Context, tenantID, orderID, reason string, detail []byte) error
}

// Notifier 出站通知契约（S3 卡片；M7 由 wecom.Gateway 实现，L2 用 fakeNotifier）。
type Notifier interface {
	// NotifyApproval 审批待办/催办通知。
	NotifyApproval(ctx context.Context, tenantID, orderID, kind, assignee string, taskID string) error
	// NotifyPreOrderConfirm 预订单确认卡片（S2）。
	NotifyPreOrderConfirm(ctx context.Context, tenantID, preOrderID string, candidates []byte) error
	// NotifyOrderEvent 状态通知（S3）。
	NotifyOrderEvent(ctx context.Context, tenantID, orderID, state string) error
}

// AuthChecker 授权判定契约（独立验证者，每个业务副作用前重复验证；L2-14）。
type AuthChecker interface {
	Check(ctx context.Context, req authz.CheckRequest) (bool, error)
}

// Deps 工作流依赖聚合。
type Deps struct {
	Store    Store
	Data     orderdata.Provider
	Notifier Notifier
	Authz    AuthChecker
}
