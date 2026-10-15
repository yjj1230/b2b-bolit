// Package authz —— 授权层（docs/02-architecture.html §2.3）
//
// 唯一授权判定点：所有权限判定走 Checker → SpiceDB CheckPermission（FullyConsistent）；
// 应用代码零硬编码授权。StaticChecker 仅为测试替身/降级模式（L0-09）。
package authz

import (
	"context"
	"fmt"
	"time"
)

// Permission 权限名（与 schema.zed permission 一一对应）。
type Permission string

const (
	PermView           Permission = "view"            // 读（数据可见范围，CQ-02）
	PermEdit           Permission = "edit"            // 写（提交/修改）
	PermApprove        Permission = "approve"         // 审批
	PermStock          Permission = "stock"           // 商务备货
	PermShip           Permission = "ship"            // 发货
	PermConfirmPayment Permission = "confirm_payment" // 财务确认到账
	// ── rebac_cases_spicedb.html 细粒度权限（D3 决策；场景 A–H）──
	PermCreatePrestock  Permission = "create_prestock"  // 场景 A：开备货单
	PermChangeQuota     Permission = "change_quota"     // 场景 B：改备货额度
	PermRequestShipment Permission = "request_shipment" // 场景 B：发起发货
	PermClose           Permission = "close"            // 场景 B：关闭备货单
	PermConfirm         Permission = "confirm"          // 场景 C：仓管确认发货
	PermReject          Permission = "reject"           // 场景 C：仓管驳回
	PermAppendEvidence  Permission = "append_evidence"  // 场景 D：追加凭证
	PermVoid            Permission = "void"             // 场景 F：作废（阶段二窗口）
)

// ResourceType 资源类型（与 schema.zed definition 一一对应）。
type ResourceType string

const (
	ResOrder     ResourceType = "order"
	ResCustomer  ResourceType = "customer"
	ResWarehouse ResourceType = "warehouse"
	ResTenant    ResourceType = "tenant"
	ResUser      ResourceType = "user"
	// rebac 场景一等授权对象（D1 决策）
	ResPrestock ResourceType = "prestock"
	ResShipment ResourceType = "shipment"
	ResEvidence ResourceType = "evidence"
)

// CheckRequest 授权判定请求。
type CheckRequest struct {
	Permission   Permission
	ResourceType ResourceType
	ResourceID   string
	SubjectID    string // 用户 ID（租户内唯一）
	// Now 当前时间（RFC3339）。SpiceDB caveat `expiry_ok` 的 CEL 环境没有内置时钟，
	// 需要调用方在判定时注入 `now`（CQ-13 时效判定的单一时间源）。
	Now *time.Time
}

// Relationship 授权关系（图拓扑变更 = Write/Delete）。
type Relationship struct {
	ResourceType ResourceType
	ResourceID   string
	Relation     string // owner/delegate/reviewer/customer/tenant/warehouse/staff/ops/accounting/admin
	SubjectType  ResourceType
	SubjectID    string
}

// Checker 授权判定接口。
type Checker interface {
	Check(ctx context.Context, req CheckRequest) (bool, error)
}

// ── StaticChecker：角色×动作对照表（L0-09；测试替身/降级模式）──
//
// 与 schema.zed 语义对齐的简化 RBAC 判定：仅供单元测试与 mock 演示模式使用，
// 生产判定必须走 SpiceDB（FullyConsistent）。

// Role 角色枚举（与 users.role 一致）。
type Role string

const (
	RoleSales      Role = "SALES"
	RoleSupervisor Role = "SUPERVISOR"
	RoleChairman   Role = "CHAIRMAN"
	RoleOP         Role = "OP"
	RoleAccounting Role = "ACCOUNTING"
	RoleWarehouse  Role = "WAREHOUSE"
	RoleAdmin      Role = "ADMIN"
	// rebac 场景 D/H：客户联系人（物流视图角色）
	RoleContact Role = "CONTACT"
)

// StaticRoleResolver 用户 → 角色解析（由调用方注入）。
type StaticRoleResolver func(userID string) (role Role, err error)

// StaticChecker 静态授权判定（对照表驱动，零依赖）。
type StaticChecker struct {
	Resolver StaticRoleResolver
}

// roleAllows 角色 × 动作矩阵（与 schema.zed 语义对齐；演示模式简化，SpiceDB 才是唯一判定点）。
func roleAllows(role Role, perm Permission) bool {
	switch perm {
	case PermView, PermEdit:
		// 数据范围（谁可见哪些单）由资源级关系决定，StaticChecker 只在资源归属
		// 已由调用方判定后做角色门禁（演示模式简化）；此处全角色可 view/edit
		// 不等于生产语义 —— SpiceDB 才是唯一判定点。
		return true
	case PermApprove:
		return role == RoleSupervisor || role == RoleChairman || role == RoleAdmin
	case PermStock:
		return role == RoleOP || role == RoleAdmin
	case PermShip:
		return role == RoleOP || role == RoleWarehouse || role == RoleAdmin
	case PermConfirmPayment:
		return role == RoleAccounting || role == RoleAdmin
	// ── rebac 场景细粒度（D3；角色为粗略模板，对象级判定走 SpiceDB）──
	case PermCreatePrestock, PermChangeQuota, PermRequestShipment, PermClose:
		return role == RoleOP || role == RoleAdmin
	case PermConfirm, PermReject:
		return role == RoleWarehouse || role == RoleAdmin
	case PermAppendEvidence:
		// 0.1 A2 主路径：财务订单后台上传凭证（CW1）；仓管回单（CG3）；商务/管理员
		return role == RoleOP || role == RoleWarehouse || role == RoleAccounting || role == RoleAdmin
	case PermVoid:
		return role == RoleOP || role == RoleAdmin
	}
	return false
}

// Check 静态判定（演示/测试模式）。
func (s *StaticChecker) Check(ctx context.Context, req CheckRequest) (bool, error) {
	role, err := s.Resolver(req.SubjectID)
	if err != nil {
		return false, fmt.Errorf("resolve role for %s: %w", req.SubjectID, err)
	}
	return roleAllows(role, req.Permission), nil
}

// NewStaticChecker 构造带默认解析器的 StaticChecker（L0-09 单测用）。
func NewStaticChecker(resolver StaticRoleResolver) *StaticChecker {
	return &StaticChecker{Resolver: resolver}
}
