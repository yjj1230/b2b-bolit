package domain

// ShpFSM —— SHP 发货分配及出库流程状态机（docs/01-core-graph.html §2.4）。
//
// 发货单纪律：一张发货单只发一次（OPEN→SHIPPED→CLOSED）；少发回退备货层。
// 实施校准（docs/05-roundtrip-report.html §3）：SHIP_ASSIGNING → SHIPPED 直连边——
// ship-action 信号在 SHIP_ASSIGNING 闸门直接执行「实发完成」（发货+库存扣减单事务），
// 中间态 WAREHOUSE_SHIPPING 保留给退回/重入路径。
var ShpFSM = NewFSM("SHP", StateSHPShipAssigning, map[string][]string{
	StateSHPShipAssigning: {
		StateSHPWarehouseShip,    // 提交仓管实际发货（中间态；重入/退回路径）
		StateSHPShipPed,          // ✅ 实发完成（信号直达：发货单+库存扣减单事务，实施校准）
		StateSTKWarehouseConfirm, // 仓管申请转仓（未发货即转；与 PARTIAL_SHIPPED 的转仓边同语义）
	},
	StateSHPWarehouseShip: {
		StateSHPShipPed,       // ✅ 实际发货完成（发货单+库存扣减单事务）
		StateSHPShipAssigning, // 仓管退回（少发→回退备货层）
	},
	StateSHPShipPed: {
		StateSHPFullyShipped,   // 累计发货 = 订单总量
		StateSHPPartialShipped, // 累计发货 < 订单总量
	},
	StateSHPPartialShipped: {
		StateSHPShipAssigning,    // 按原分批计划继续
		StateSTKWarehouseConfirm, // 仓管申请退回/转仓（回 STK 段）
		StateINVReadyForInvoice,  // 已发部分申请开票
	},
	StateSHPFullyShipped: {
		StateINVReadyForInvoice, // 全部发货完成
	},
})
