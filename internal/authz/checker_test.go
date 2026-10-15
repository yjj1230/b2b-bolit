package authz

import (
	"context"
	"errors"
	"testing"
	"time"
)

// L0-09 · 授权决策纯函数（角色×动作矩阵，StaticChecker 对照表；CQ-01）
func TestL009StaticCheckerMatrix(t *testing.T) {
	roles := map[string]Role{
		"alice-sales": RoleSales,
		"bob-super":   RoleSupervisor,
		"carol-chair": RoleChairman,
		"dave-op":     RoleOP,
		"erin-acct":   RoleAccounting,
		"frank-wh":    RoleWarehouse,
		"grace-admin": RoleAdmin,
	}
	sc := NewStaticChecker(func(userID string) (Role, error) {
		r, ok := roles[userID]
		if !ok {
			return "", errors.New("unknown user")
		}
		return r, nil
	})
	ctx := context.Background()

	cases := []struct {
		user string
		perm Permission
		want bool
	}{
		// 审批：主管/董事长/管理员
		{"bob-super", PermApprove, true},
		{"carol-chair", PermApprove, true},
		{"grace-admin", PermApprove, true},
		{"alice-sales", PermApprove, false},
		{"dave-op", PermApprove, false},
		{"frank-wh", PermApprove, false},
		// 备货：商务/管理员
		{"dave-op", PermStock, true},
		{"grace-admin", PermStock, true},
		{"alice-sales", PermStock, false},
		// 发货：商务/仓管/管理员
		{"dave-op", PermShip, true},
		{"frank-wh", PermShip, true},
		{"grace-admin", PermShip, true},
		{"alice-sales", PermShip, false},
		{"erin-acct", PermShip, false},
		// 财务确认到账：财务/管理员
		{"erin-acct", PermConfirmPayment, true},
		{"grace-admin", PermConfirmPayment, true},
		{"dave-op", PermConfirmPayment, false},
		// 未知用户 → 错误
		{"ghost", PermView, false},
	}
	for _, c := range cases {
		got, err := sc.Check(ctx, CheckRequest{Permission: c.perm, SubjectID: c.user})
		if c.user == "ghost" {
			if err == nil {
				t.Errorf("ghost user should error")
			}
			continue
		}
		if err != nil {
			t.Fatalf("check %s#%s: %v", c.user, c.perm, err)
		}
		if got != c.want {
			t.Errorf("role matrix: %s#%s = %v, want %v", c.user, c.perm, got, c.want)
		}
	}

	// 未知权限 → 拒绝（对照表无此列）
	unknownPerm := Permission("bogus")
	got, err := sc.Check(ctx, CheckRequest{Permission: unknownPerm, SubjectID: "grace-admin"})
	if err != nil {
		t.Fatalf("unknown perm check: %v", err)
	}
	if got {
		t.Error("unknown permission should be denied")
	}
}

// SpiceDB 客户端错误路径（无服务：连接失败/检查失败/关系写失败/schema 文件缺失）
func TestSpiceDBClientErrors(t *testing.T) {
	ctx := context.Background()
	// grpc.NewClient 懒连接：构造不报错；首次 RPC 报错
	c, err := NewSpiceDB("127.0.0.1:1", "key")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, err := c.Check(ctx, CheckRequest{
		Permission: PermView, ResourceType: ResOrder, ResourceID: "o", SubjectID: "u",
	}); err == nil {
		t.Fatal("check against dead server should fail")
	}
	if err := c.TouchRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: "o", Relation: "owner", SubjectType: ResUser, SubjectID: "u"}); err == nil {
		t.Fatal("touch against dead server should fail")
	}
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: "o", Relation: "owner", SubjectType: ResUser, SubjectID: "u"}); err == nil {
		t.Fatal("delete against dead server should fail")
	}
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: "o", Relation: "delegate", SubjectType: ResUser, SubjectID: "u"}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("caveated write against dead server should fail")
	}
	// schema 文件缺失
	if err := c.ApplySchemaFromFile(ctx, "/nonexistent/schema.zed"); err == nil {
		t.Fatal("missing schema file should fail")
	}
}

// rebac 8 细粒度权限的角色矩阵（D3 扩展后的 roleAllows 全分支）
func TestRoleAllowsReBACPermissions(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	// 直接用 roleAllows 纯函数穷举
	matrix := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleOP, PermCreatePrestock, true}, {RoleAdmin, PermCreatePrestock, true},
		{RoleSales, PermCreatePrestock, false}, {RoleWarehouse, PermCreatePrestock, false},
		{RoleOP, PermChangeQuota, true}, {RoleWarehouse, PermChangeQuota, false},
		{RoleOP, PermRequestShipment, true}, {RoleAccounting, PermRequestShipment, false},
		{RoleOP, PermClose, true}, {RoleSupervisor, PermClose, false},
		{RoleWarehouse, PermConfirm, true}, {RoleOP, PermConfirm, false},
		{RoleWarehouse, PermReject, true}, {RoleChairman, PermReject, false},
		{RoleOP, PermAppendEvidence, true}, {RoleWarehouse, PermAppendEvidence, true},
		{RoleSales, PermAppendEvidence, false},
		{RoleOP, PermVoid, true}, {RoleAccounting, PermVoid, false},
		{RoleAdmin, PermVoid, true},
		// 既有权限回归
		{RoleOP, PermStock, true}, {RoleWarehouse, PermShip, true},
		{RoleAccounting, PermConfirmPayment, true}, {RoleWarehouse, PermApprove, false},
	}
	for _, c := range matrix {
		if got := roleAllows(c.role, c.perm); got != c.want {
			t.Errorf("roleAllows(%s,%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}
