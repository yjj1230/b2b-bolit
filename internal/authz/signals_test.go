package authz

import "testing"

func TestSignalRoleGateMatrix(t *testing.T) {
	// 审价/终审：审批角色专属
	if !CanSignal(RoleSupervisor, "price-review") || !CanSignal(RoleAdmin, "price-review") {
		t.Fatal("supervisor/admin must signal price-review")
	}
	if CanSignal(RoleOP, "price-review") || CanSignal(RoleWarehouse, "price-review") {
		t.Fatal("OP/warehouse must not signal price-review")
	}
	if !CanSignal(RoleChairman, "chairman-review") || CanSignal(RoleSupervisor, "chairman-review") {
		t.Fatal("chairman-review gate wrong")
	}
	// 备货/转仓：商务专属
	if !CanSignal(RoleOP, "stock-review") || !CanSignal(RoleOP, "transfer-decision") {
		t.Fatal("OP must signal stock/transfer")
	}
	if CanSignal(RoleSales, "stock-review") {
		t.Fatal("sales must not signal stock-review")
	}
	// 0.1 商务承接口径：SHP/INV 名义角色动作允许 OP 代办
	for _, sig := range []string{"warehouse-confirm", "ship-action", "receipt-provided",
		"payment-confirm", "receipt-ex-review", "invoice-issued", "payment-tracked"} {
		if !CanSignal(RoleOP, sig) {
			t.Fatalf("OP must signal %s (0.1 商务承接)", sig)
		}
	}
	// 财务专属到账确认也允许财务
	if !CanSignal(RoleAccounting, "payment-confirm") || CanSignal(RoleChairman, "payment-confirm") {
		t.Fatal("payment-confirm gate wrong")
	}
	// 修改/重提：销售/商务
	if !CanSignal(RoleSales, "modify-order") || !CanSignal(RoleSales, "resubmit") {
		t.Fatal("sales must modify/resubmit")
	}
	// 未知信号一律拒绝
	if CanSignal(RoleAdmin, "no-such-signal") {
		t.Fatal("unknown signal must be denied")
	}
}

func TestCreateAndPreOrderGates(t *testing.T) {
	if !CanCreateOrder(RoleSales) || !CanCreateOrder(RoleOP) || !CanCreateOrder(RoleAdmin) {
		t.Fatal("sales/op/admin may create")
	}
	if CanCreateOrder(RoleWarehouse) || CanCreateOrder(RoleAccounting) {
		t.Fatal("warehouse/accounting must not create")
	}
	if !CanPreOrderSignal(RoleSales) || !CanPreOrderSignal(RoleAdmin) {
		t.Fatal("sales/admin may confirm preorder")
	}
	if CanPreOrderSignal(RoleOP) || CanPreOrderSignal(RoleSupervisor) {
		t.Fatal("only sales confirms preorder (N-02)")
	}
}
