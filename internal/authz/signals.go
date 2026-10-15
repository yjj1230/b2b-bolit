// Package authz —— 授权层：v0.1 信号级角色闸门（RBAC 前置防线）
//
// 依据 docs/v0.1/08-api §2「权限：按信号对应 permission」与 v0.1 发布口径
// 「仅虹励（商务）操作，财务/仓管名义角色由商务承接走通」。
// 这里是 HTTP API 的第一道角色门禁（对象级判定仍由 SpiceDB Checker 承担，见
// handleGetOrder/handleAppendEvidence 等处的 Check 调用）。生产部署中 cmd/api
// 的 StrictIdentity 保证角色来自服务端签发的 X-WeCom-Context 而非裸请求头。
package authz

// CanCreateOrder 是否允许创建订单（v0.1：销售本人/代办（OP）/管理员）。
func CanCreateOrder(role Role) bool {
	switch role {
	case RoleSales, RoleOP, RoleAdmin:
		return true
	default:
		return false
	}
}

// SignalRoleGate 订单信号 → 允许角色矩阵（AUTH-01 收敛）。
// v0.1 商务承接口径：SHP/INV 的名义角色（仓管/财务）动作允许 OP 代为执行；
// 审价/终审保持审批角色专属，任何信号都不可由无关角色投递。
var signalRoleGate = map[string][]Role{
	"price-review":      {RoleSupervisor, RoleAdmin},
	"chairman-review":   {RoleChairman, RoleAdmin},
	"stock-review":      {RoleOP, RoleAdmin},
	"warehouse-confirm": {RoleWarehouse, RoleOP, RoleAdmin}, // 0.1 商务承接发货
	"transfer-decision": {RoleOP, RoleAdmin},
	"payment-confirm":   {RoleAccounting, RoleOP, RoleAdmin}, // 0.1 商务承接
	"ship-action":       {RoleWarehouse, RoleOP, RoleAdmin},  // 0.1 商务承接
	"receipt-ex-review": {RoleAccounting, RoleOP, RoleAdmin}, // 0.1 商务承接
	"receipt-provided":  {RoleWarehouse, RoleOP, RoleAdmin},
	"invoice-issued":    {RoleAccounting, RoleOP, RoleAdmin}, // 0.1 商务承接
	"payment-tracked":   {RoleAccounting, RoleOP, RoleAdmin},
	"modify-order":      {RoleSales, RoleOP, RoleAdmin},
	"resubmit":          {RoleSales, RoleOP, RoleAdmin},
}

// CanSignal 订单信号是否允许该角色投递。
func CanSignal(role Role, signal string) bool {
	allowed, ok := signalRoleGate[signal]
	if !ok {
		return false
	}
	for _, r := range allowed {
		if role == r {
			return true
		}
	}
	return false
}

// CanPreOrderSignal 预订单确认/忽略：销售是唯一可确认角色（N-02）。
func CanPreOrderSignal(role Role) bool {
	return role == RoleSales || role == RoleAdmin
}
