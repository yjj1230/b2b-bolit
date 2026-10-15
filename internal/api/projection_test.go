package api

import (
	"testing"

	"b2b_pilot_v3/internal/authz"
)

// L1-10 · 仓管只读投影：本仓订单可访问（授权层验证）、响应剥离价格/金额（BFF 投影；CQ-02/03）
// 授权判定部分由 L1-07 覆盖（仓管 view ✓ / 跨仓 ✗）；此处验证投影纯函数。
func TestL110WarehouseProjection(t *testing.T) {
	items := []OrderItemView{{ProductID: "prod-c68", Quantity: 10}}
	const up, am = int64(1000), int64(10000)

	// 仓管视图：价格/金额剥离
	wh := ProjectOrder("ORD-1", "SHP_SHIPPED", "款到", "渠道A", nil, items, up, am, authz.RoleWarehouse)
	if wh.UnitPriceCents != nil || wh.AmountCents != nil {
		t.Fatal("warehouse view must strip unit_price_cents and amount_cents")
	}
	if wh.ID != "ORD-1" || wh.State != "SHP_SHIPPED" || len(wh.Items) != 1 {
		t.Fatal("warehouse view must keep fulfillment fields")
	}
	if wh.Items[0].Quantity != 10 {
		t.Fatal("quantity must remain visible to warehouse")
	}

	// 销售/商务视图：保留价格
	for _, role := range []authz.Role{authz.RoleSales, authz.RoleOP, authz.RoleSupervisor, authz.RoleAdmin} {
		v := ProjectOrder("ORD-1", "ORD_CONTRACTED", "款到", "渠道A", nil, items, up, am, role)
		if v.UnitPriceCents == nil || *v.UnitPriceCents != up {
			t.Fatalf("role %s must see unit price", role)
		}
		if v.AmountCents == nil || *v.AmountCents != am {
			t.Fatalf("role %s must see amount", role)
		}
	}
	// 合同号保留
	cn := "HT-2026-001"
	v := ProjectOrder("ORD-1", "ORD_CONTRACTED", "款到", "渠道A", &cn, items, up, am, authz.RoleSales)
	if v.ContractNo == nil || *v.ContractNo != cn {
		t.Fatal("contract_no must be preserved")
	}
}

// rebac 场景 D：客户联系人物流视图（剥离价格/内部备注；凭证版本只追加）
func TestShipmentProjectionContactView(t *testing.T) {
	evidence := []EvidenceVersionView{{Version: 1, Uploader: "li-customer", UploadedAt: "2026-08-06T10:00:00Z"}}
	// 客户联系人：无价格/内部备注，有物流状态与凭证
	contact := ProjectShipment("SHP-02", "ORD-1", "SHIPPED", 100, 100, 100000, "内部备注", evidence, authz.RoleContact)
	if contact.AmountCents != nil || contact.InternalNote != nil {
		t.Fatal("contact view must strip price and internal note")
	}
	if contact.State != "SHIPPED" || contact.ShippedQty != 100 {
		t.Fatal("contact view must keep logistics fields")
	}
	if len(contact.EvidenceVersions) != 1 || contact.EvidenceVersions[0].Version != 1 {
		t.Fatal("contact view must expose evidence versions (CQ-15)")
	}
	// 仓管：同样剥离价格（但保留履约字段）
	wh := ProjectShipment("SHP-02", "ORD-1", "SHIPPED", 100, 100, 100000, "内部备注", nil, authz.RoleWarehouse)
	if wh.AmountCents != nil || wh.InternalNote != nil {
		t.Fatal("warehouse view must strip price and internal note")
	}
	// 商务/销售：保留价格与备注
	for _, role := range []authz.Role{authz.RoleOP, authz.RoleSales, authz.RoleAdmin} {
		v := ProjectShipment("SHP-02", "ORD-1", "SHIPPED", 100, 100, 100000, "内部备注", nil, role)
		if v.AmountCents == nil || *v.AmountCents != 100000 || v.InternalNote == nil {
			t.Fatalf("role %s must see price and note", role)
		}
	}
}
