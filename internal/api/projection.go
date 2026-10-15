// Package api —— HTTP handler、请求校验、幂等启动、字段投影（BFF）
package api

import "b2b_pilot_v3/internal/authz"

// ── BFF 字段级投影（CQ-03：仓管不见价格；docs/02-architecture.html §2.3）──
// 授权判定是唯一前置：SpiceDB 判定「本仓仓管可读」，BFF 再按角色剥离价格/金额字段。

// OrderView 订单查询视图（投影结果）。
type OrderView struct {
	ID              string          `json:"id"`
	State           string          `json:"state"`
	PaymentTerm     string          `json:"payment_term"`
	SupplierChannel string          `json:"supplier_channel"`
	ContractNo      *string         `json:"contract_no,omitempty"`
	Items           []OrderItemView `json:"items"`
	// 价格字段（仓管视图剥离；非仓管保留）
	UnitPriceCents *int64 `json:"unit_price_cents,omitempty"`
	AmountCents    *int64 `json:"amount_cents,omitempty"`
}

// OrderItemView 明细视图。
type OrderItemView struct {
	ProductID string `json:"product_id"`
	Quantity  int64  `json:"quantity"`
	// ReceiveAddress 收货地址（A1：备货选仓依据；对全部角色可见）
	ReceiveAddress string `json:"receive_address"`
}

// ProjectOrder 字段投影：role 为 WAREHOUSE / CONTACT 时剥离价格/金额字段
// （CQ-03 仓管；07 §4.1 客户联系人剥金额——场景 D 一致语义）。
func ProjectOrder(orderID, state, paymentTerm, supplierChannel string, contractNo *string,
	items []OrderItemView, unitPriceCents, amountCents int64, role authz.Role) OrderView {
	v := OrderView{
		ID: orderID, State: state, PaymentTerm: paymentTerm, SupplierChannel: supplierChannel,
		ContractNo: contractNo, Items: items,
	}
	if role == authz.RoleWarehouse || role == authz.RoleContact {
		return v // 仓管/客户联系人视图：无任何价格/金额字段
	}
	up, am := unitPriceCents, amountCents
	v.UnitPriceCents = &up
	v.AmountCents = &am
	return v
}

// ── rebac 场景 D：客户联系人物流视图（BFF 投影扩展；决策 D3/D4）──

// ShipmentView 发货单物流视图（场景 D：客户联系人只见物流状态/凭证，不见价格/内部备注）。
type ShipmentView struct {
	ShipmentID string `json:"shipment_id"`
	OrderID    string `json:"order_id"`
	State      string `json:"state"`
	ShippedQty int64  `json:"shipped_qty"`
	PlannedQty int64  `json:"planned_qty"`
	// 价格/内部字段（仓管与客户联系人视图均剥离）
	AmountCents  *int64  `json:"amount_cents,omitempty"`
	InternalNote *string `json:"internal_note,omitempty"`
	// 凭证（只追加版本，CQ-15）
	EvidenceVersions []EvidenceVersionView `json:"evidence_versions,omitempty"`
}

// EvidenceVersionView 凭证版本（只追加）。
type EvidenceVersionView struct {
	Version    int    `json:"version"`
	Uploader   string `json:"uploader"`
	UploadedAt string `json:"uploaded_at"`
}

// ProjectShipment 发货单投影：CONTACT/VIEWER 角色剥离价格与内部备注。
func ProjectShipment(shipmentID, orderID, state string, shippedQty, plannedQty int64,
	amountCents int64, internalNote string, evidence []EvidenceVersionView, role authz.Role) ShipmentView {
	v := ShipmentView{
		ShipmentID: shipmentID, OrderID: orderID, State: state,
		ShippedQty: shippedQty, PlannedQty: plannedQty,
		EvidenceVersions: evidence,
	}
	switch role {
	case authz.RoleWarehouse, authz.RoleContact:
		// 仓管/客户联系人：剥离价格与内部备注
		return v
	}
	am := amountCents
	note := internalNote
	v.AmountCents = &am
	v.InternalNote = &note
	return v
}
