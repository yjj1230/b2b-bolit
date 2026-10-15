package domain

// InvFSM —— INV 开票回单审批流程状态机（docs/01-core-graph.html §2.5）。
//
// 回单不卡开票：回单状态（CQ-10）与开票状态解耦，仅作例外审核触发条件；
// 开票守恒（CQ-09）在 INVOICING → INVOICED 事务内校验（开票量按有效发货核验）。
var InvFSM = NewFSM("INV", StateINVReadyForInvoice, map[string][]string{
	StateINVReadyForInvoice: {
		StateINVInvoiceApplying,    // 分流①款到发货直接进入 / 分流②有回单
		StateINVReceiptExReviewing, // 分流③无回单超 5 天（Timer）/ 分流④提前申请
	},
	StateINVReceiptExReviewing: {
		StateINVInvoiceApplying, // ✅ 例外审核通过（Signal）
		StateINVWaitingReceipt,  // ❌ 例外拒绝→待补回单
	},
	StateINVWaitingReceipt: {
		StateINVInvoiceApplying, // 补回单后进入开票
	},
	StateINVInvoiceApplying: {
		StateINVInvoicing, // 提交开票申请→推送财务
	},
	StateINVInvoicing: {
		StateINVInvoiced,        // ✅ 财务确认开票完成（Signal，开票守恒校验）
		StateINVInvoiceFailed,   // ❌ 开票失败（可重试）
		StateINVNoInvoiceNeeded, // 无需开票（业务判定）
	},
	StateINVInvoiced: {
		StateINVPaymentTracking, // 回款跟踪
	},
	StateINVNoInvoiceNeeded: {
		StateINVPaymentTracking, // 直接进入回款跟踪
	},
	StateINVInvoiceFailed: {
		StateINVInvoicing, // 重试开票
	},
	StateINVPaymentTracking: {
		StateINVContractArchived, // 回款登记/合同资料归档
	},
	StateINVContractArchived: {
		StateINVClosed, // 订单关闭
	},
})
