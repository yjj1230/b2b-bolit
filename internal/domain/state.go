// Package domain —— 领域层（纯 Go，零外部依赖）
//
// 设计来源：docs/01-core-graph.html §2（任务图谱四流程状态机 + 预订单状态机）与
// docs/02-architecture.html（值对象/守恒断言/幂等键）。
// 纪律：任何状态迁移都必须经过本包 FSM 校验；数量守恒基准只在本包定义。
package domain

// ── 四流程 + 预订单状态常量（单一事实源，docs/01-core-graph.html §2.2-2.6）──
//
// orders.state 为四分支联合状态，故常量带阶段前缀避免跨阶段同名冲突
// （如 ORD 与 STK 均有 RETURNED；ORD 与 STK 交界处共享 READY_FOR_*）。
const (
	// ORD 段（§2.2）
	StateORDRequested        = "ORD_REQUESTED"
	StateORDPriceReviewing   = "ORD_PRICE_REVIEWING"
	StateORDPriceApproved    = "ORD_PRICE_APPROVED"
	StateORDChairmanReview   = "ORD_CHAIRMAN_REVIEWING"
	StateORDContractGenerate = "ORD_CONTRACT_GENERATING"
	StateORDContracted       = "ORD_CONTRACTED"
	StateORDModifying        = "ORD_MODIFYING"
	StateORDReturned         = "ORD_RETURNED"
	StateORDRejected         = "ORD_REJECTED"

	// STK 段（§2.3）
	StateSTKReadyForStock      = "STK_READY_FOR_STOCK"
	StateSTKStockReviewing     = "STK_STOCK_REVIEWING"
	StateSTKWarehouseConfirm   = "STK_WAREHOUSE_CONFIRMING"
	StateSTKWarehouseConfirmed = "STK_WAREHOUSE_CONFIRMED"
	StateSTKWarehouseReturned  = "STK_WAREHOUSE_RETURNED"
	StateSTKTransferRequested  = "STK_TRANSFER_REQUESTED"
	StateSTKPaymentConfirming  = "STK_PAYMENT_CONFIRMING"
	StateSTKPaymentConfirmed   = "STK_PAYMENT_CONFIRMED"
	StateSTKShipOrderIssued    = "STK_SHIP_ORDER_ISSUED"
	StateSTKReturned           = "STK_RETURNED"

	// SHP 段（§2.4）
	StateSHPShipAssigning  = "SHP_SHIP_ASSIGNING"
	StateSHPWarehouseShip  = "SHP_WAREHOUSE_SHIPPING"
	StateSHPShipPed        = "SHP_SHIPPED"
	StateSHPFullyShipped   = "SHP_FULLY_SHIPPED"
	StateSHPPartialShipped = "SHP_PARTIAL_SHIPPED"

	// INV 段（§2.5）
	StateINVReadyForInvoice    = "INV_READY_FOR_INVOICE"
	StateINVInvoiceApplying    = "INV_INVOICE_APPLYING"
	StateINVReceiptExReviewing = "INV_RECEIPT_EXCEPTION_REVIEWING"
	StateINVWaitingReceipt     = "INV_WAITING_RECEIPT"
	StateINVInvoicing          = "INV_INVOICING"
	StateINVInvoiced           = "INV_INVOICED"
	StateINVInvoiceFailed      = "INV_INVOICE_FAILED"
	StateINVNoInvoiceNeeded    = "INV_NO_INVOICE_NEEDED"
	StateINVPaymentTracking    = "INV_PAYMENT_TRACKING"
	StateINVContractArchived   = "INV_CONTRACT_ARCHIVED"
	StateINVClosed             = "INV_CLOSED"

	// 预订单（§2.6）
	StatePreCaptured       = "PRE_CAPTURED"
	StatePreIntentParsing  = "PRE_INTENT_PARSING"
	StatePreCollecting     = "PRE_COLLECTING"
	StatePrePendingConfirm = "PRE_PENDING_CONFIRM"
	StatePreAmbiguous      = "PRE_AMBIGUOUS"
	StatePreConfirmed      = "PRE_CONFIRMED"
	StatePreModified       = "PRE_MODIFIED"
	StatePreEscalated      = "PRE_ESCALATED"
	StatePreExpired        = "PRE_EXPIRED"
	StatePreIgnored        = "PRE_IGNORED"
	StatePreOrderRequested = "PRE_ORDER_REQUESTED"
)

// ── 聚合子状态（发货单/发票/审批任务自身的状态枚举，02-architecture.md §3.1）──
const (
	ShipmentStateOpen    = "OPEN"
	ShipmentStateShipped = "SHIPPED"
	ShipmentStateClosed  = "CLOSED"

	InvoiceStatePending    = "PENDING"
	InvoiceStateProcessing = "PROCESSING"
	InvoiceStateIssued     = "ISSUED"
	InvoiceStateFailed     = "FAILED"
	InvoiceStateNone       = "NONE"
)

// Stage 返回状态所属阶段前缀（ORD/STK/SHP/INV/PRE）。
func Stage(s string) string {
	if len(s) < 4 {
		return ""
	}
	return s[:3]
}

// IsTerminal 判断状态是否为终态（流程不可再推进）。
func IsTerminal(s string) bool {
	switch s {
	case StateORDRejected, StatePreIgnored, StatePreExpired, StateINVClosed:
		return true
	}
	return false
}
