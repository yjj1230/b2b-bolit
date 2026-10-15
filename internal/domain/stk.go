package domain

// StkFSM —— STK 备货审批流程状态机（docs/01-core-graph.html §2.3）。
var StkFSM = NewFSM("STK", StateSTKReadyForStock, map[string][]string{
	StateSTKReadyForStock: {
		StateSTKStockReviewing, // 款到发货（商务备货：选仓+发货量）
		StateSHPShipAssigning,  // 非款到发货（跳过备货审批，直接发货分配）
	},
	StateSTKStockReviewing: {
		StateSTKWarehouseConfirm, // 商务备货通过（Signal stock-review）→ 提交仓管确认
		StateSTKReturned,         // 商务备货驳回（Signal）→ 通知销售
	},
	StateSTKWarehouseConfirm: {
		StateSTKWarehouseConfirmed, // ✅ 仓管确认可执行（Signal）
		StateSTKWarehouseReturned,  // 仓管退回（Signal）
		StateSTKTransferRequested,  // 仓管申请转仓（Signal）
	},
	StateSTKWarehouseReturned: {
		StateSTKWarehouseConfirm, // 历史兼容：退回后重确认（0.1 主路径已改为重开备货）
		StateSTKStockReviewing,   // D-02：仓管退回 → 商务重新选仓/调交期
	},
	StateSTKTransferRequested: {
		StateSTKWarehouseConfirm,   // 商务审核同意转仓（换仓重确认）
		StateSTKWarehouseConfirmed, // 商务审核同意原仓
		StateSTKReturned,           // 商务驳回转仓（退回修改）
	},
	StateSTKWarehouseConfirmed: {
		StateSTKPaymentConfirming, // 款到发货（财务确认到账，按供方分流）
		StateSTKShipOrderIssued,   // 非款到发货
	},
	StateSTKPaymentConfirming: {
		StateSTKPaymentConfirmed, // ✅ 财务确认到账（Signal）
		StateSTKReturned,         // 未到账→退回商务
	},
	StateSTKPaymentConfirmed: {
		StateSTKShipOrderIssued, // 商务发出发货命令
	},
	StateSTKShipOrderIssued: {
		StateSHPShipAssigning, // 进入发货段（商务选仓+发货量）
	},
	StateSTKReturned: {
		StateSTKStockReviewing, // 修改后重提
	},
})
