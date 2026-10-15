//go:build integration

package authz

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── L1 测试环境：spicedb serve-testing（内存模式，docs/03-testing.html §5）──

// startTestServer 启动 spicedb 测试服务器并应用 schema.zed（编译门禁）。
func startTestServer(t *testing.T) *SpiceDBClient {
	t.Helper()
	if _, err := exec.LookPath("spicedb"); err != nil {
		t.Skipf("spicedb binary not found — L1 authz test skipped (degraded mode)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", port)

	args := [][]string{
		{"serve-testing", "--grpc-addr", grpcAddr, "--readonly-grpc-enabled=false", "--http-enabled=false", "--log-level", "error"},
		{"testserver", "--grpc-addr", grpcAddr, "--log-level", "error"},
	}
	var cmd *exec.Cmd
	for _, a := range args {
		c := exec.Command("spicedb", a...)
		if err := c.Start(); err != nil {
			continue
		}
		cmd = c
		break
	}
	if cmd == nil {
		t.Fatalf("start spicedb testserver: no working command")
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ready := false
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", grpcAddr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("spicedb testserver not ready on %s", grpcAddr)
	}

	client, err := NewSpiceDB(grpcAddr, config.Load().SpiceDBKey)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx := context.Background()
	if err := client.ApplySchemaFromFile(ctx, "../../schema.zed"); err != nil {
		t.Fatalf("apply schema.zed (compile gate): %v", err)
	}
	return client
}

// writeRel 写入普通关系。
func writeRel(t *testing.T, c *SpiceDBClient, rt ResourceType, rid, relation, subject string) {
	t.Helper()
	err := c.TouchRelationship(context.Background(), Relationship{
		ResourceType: rt, ResourceID: rid, Relation: relation,
		SubjectType: ResUser, SubjectID: subject,
	})
	if err != nil {
		t.Fatalf("write %s#%s@%s: %v", rid, relation, subject, err)
	}
}

// writeSubjectRel 写入主体为其他资源类型的关系（如 order.customer -> customer:xxx）。
func writeSubjectRel(t *testing.T, c *SpiceDBClient, rt ResourceType, rid, relation string, st ResourceType, subject string) {
	t.Helper()
	err := c.TouchRelationship(context.Background(), Relationship{
		ResourceType: rt, ResourceID: rid, Relation: relation,
		SubjectType: st, SubjectID: subject,
	})
	if err != nil {
		t.Fatalf("write %s#%s@%s:%s: %v", rid, relation, st, subject, err)
	}
}

// check 便捷判定；now 非空时注入 caveat 上下文（expiry_ok 需要）。
func check(t *testing.T, c *SpiceDBClient, perm Permission, rt ResourceType, rid, subject string) bool {
	return checkAt(t, c, perm, rt, rid, subject, nil)
}

func checkAt(t *testing.T, c *SpiceDBClient, perm Permission, rt ResourceType, rid, subject string, now *time.Time) bool {
	t.Helper()
	ok, err := c.Check(context.Background(), CheckRequest{Permission: perm, ResourceType: rt, ResourceID: rid, SubjectID: subject, Now: now})
	if err != nil {
		t.Fatalf("check %s#%s@%s: %v", rid, perm, subject, err)
	}
	return ok
}

// seedOrderGraph 播种标准订单授权图（tenant ops/accounting/admin + customer owner + warehouse staff + order 关系）。
func seedOrderGraph(t *testing.T, c *SpiceDBClient, orderID string) {
	// 租户角色
	writeRel(t, c, ResTenant, "chang-an", "ops", "dave-op")
	writeRel(t, c, ResTenant, "chang-an", "accounting", "erin-acct")
	writeRel(t, c, ResTenant, "chang-an", "admin", "grace-admin")
	writeRel(t, c, ResTenant, "chang-an", "member", "alice-sales")
	// 客户归属（D2 命名：owner → account_owner）
	writeRel(t, c, ResCustomer, "cust-0001", "account_owner", "alice-sales")
	// 仓库仓管
	writeRel(t, c, ResWarehouse, "wh-yangyuan", "staff", "frank-wh")
	writeRel(t, c, ResWarehouse, "wh-dongguan", "staff", "frank-dg")
	// 订单层级边
	writeSubjectRel(t, c, ResOrder, orderID, "customer", ResCustomer, "cust-0001")
	writeSubjectRel(t, c, ResOrder, orderID, "tenant", ResTenant, "chang-an")
	writeSubjectRel(t, c, ResOrder, orderID, "warehouse", ResWarehouse, "wh-yangyuan")
	writeRel(t, c, ResOrder, orderID, "owner", "alice-sales")
	writeRel(t, c, ResOrder, orderID, "reviewer", "bob-super")
}

// L1-07 · 权限矩阵：角色×资源×动作全组合（9 类场景，含跨租户/跨仓拒绝；CQ-01/02）
func TestL107PermissionMatrix(t *testing.T) {
	c := startTestServer(t)
	ctx := context.Background()
	seedOrderGraph(t, c, "ORD-1001")
	const order = "ORD-1001"

	// 场景 1：销售本人 → view/edit ✓；他人 ✗
	if !check(t, c, PermView, ResOrder, order, "alice-sales") {
		t.Error("owner should view")
	}
	if !check(t, c, PermEdit, ResOrder, order, "alice-sales") {
		t.Error("owner should edit")
	}
	if check(t, c, PermEdit, ResOrder, order, "erin-acct") {
		t.Error("accounting must not edit")
	}
	if check(t, c, PermView, ResOrder, order, "frank-dg") {
		t.Error("other-warehouse staff must not view")
	}

	// 场景 2：商务全量（tenant->ops → view/edit/stock/ship）
	if !check(t, c, PermView, ResOrder, order, "dave-op") ||
		!check(t, c, PermEdit, ResOrder, order, "dave-op") ||
		!check(t, c, PermStock, ResOrder, order, "dave-op") ||
		!check(t, c, PermShip, ResOrder, order, "dave-op") {
		t.Error("OP should have full view/edit/stock/ship")
	}

	// 场景 3：仓管本仓 view/ship ✓；跨仓 403
	if !check(t, c, PermView, ResOrder, order, "frank-wh") {
		t.Error("warehouse staff should view own-warehouse order")
	}
	if !check(t, c, PermShip, ResOrder, order, "frank-wh") {
		t.Error("warehouse staff should ship own-warehouse order")
	}
	if check(t, c, PermShip, ResOrder, order, "frank-dg") {
		t.Error("other-warehouse staff must not ship (cross-warehouse deny)")
	}
	// 仓管不可编辑（价格字段保护的另一道门）
	if check(t, c, PermEdit, ResOrder, order, "frank-wh") {
		t.Error("warehouse staff must not edit")
	}

	// 场景 4：财务 confirm_payment ✓（tenant->accounting）
	if !check(t, c, PermConfirmPayment, ResOrder, order, "erin-acct") {
		t.Error("accounting should confirm payment")
	}
	if check(t, c, PermConfirmPayment, ResOrder, order, "dave-op") {
		t.Error("OP must not confirm payment")
	}

	// 场景 5：管理层 approve（reviewer + admin）
	if !check(t, c, PermApprove, ResOrder, order, "bob-super") {
		t.Error("reviewer should approve")
	}
	if !check(t, c, PermApprove, ResOrder, order, "grace-admin") {
		t.Error("admin should approve")
	}
	if check(t, c, PermApprove, ResOrder, order, "alice-sales") {
		t.Error("sales must not approve")
	}

	// 场景 6/7：代办有效期内 view/edit ✓；过期 ✗（caveat 时效，CQ-13 前置；now 由调用方注入）
	now := time.Now()
	future := now.Add(24 * time.Hour)
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: order, Relation: "delegate", SubjectType: ResUser, SubjectID: "carol-delegate"}, future); err != nil {
		t.Fatalf("write caveated delegate: %v", err)
	}
	if !checkAt(t, c, PermView, ResOrder, order, "carol-delegate", &now) {
		t.Error("in-term delegate should view")
	}
	if !checkAt(t, c, PermEdit, ResOrder, order, "carol-delegate", &now) {
		t.Error("in-term delegate should edit")
	}
	// 未注入 now → caveat 无法判定 → 不授予（CONDITIONAL ≠ HAS_PERMISSION）
	if check(t, c, PermView, ResOrder, order, "carol-delegate") {
		t.Error("delegate without now context must not be granted")
	}
	past := now.Add(-time.Hour)
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: order, Relation: "delegate", SubjectType: ResUser, SubjectID: "carol-expired"}, past); err != nil {
		t.Fatalf("write expired delegate: %v", err)
	}
	if checkAt(t, c, PermView, ResOrder, order, "carol-expired", &now) {
		t.Error("expired delegate must be denied (caveat)")
	}

	// 场景 8：跨租户拒绝
	writeRel(t, c, ResTenant, "zhichu", "ops", "z-op")
	if check(t, c, PermView, ResOrder, order, "z-op") {
		t.Error("cross-tenant OP must be denied")
	}
	// 场景 9：双门禁 —— 撤销即删边，立即失效
	writeRel(t, c, ResOrder, order, "reviewer", "bob-super")
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: order, Relation: "reviewer", SubjectType: ResUser, SubjectID: "bob-super"}); err != nil {
		t.Fatalf("delete reviewer: %v", err)
	}
	if check(t, c, PermApprove, ResOrder, order, "bob-super") {
		t.Error("revoked reviewer must lose approve immediately (FullyConsistent)")
	}
}

// L1-08 · ReBAC 层级：客户 owner 传播到订单（新订单自动可见；CQ-02）
func TestL108ReBACHierarchy(t *testing.T) {
	c := startTestServer(t)
	seedOrderGraph(t, c, "ORD-2001")
	// 删除订单直接 owner 边：仅靠 order.customer -> customer.owner 传播
	ctx := context.Background()
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: "ORD-2001", Relation: "owner", SubjectType: ResUser, SubjectID: "alice-sales"}); err != nil {
		t.Fatalf("delete owner: %v", err)
	}
	if !check(t, c, PermView, ResOrder, "ORD-2001", "alice-sales") {
		t.Error("customer owner must see order via ReBAC hierarchy (no direct owner edge)")
	}
	// 设计语义（schema.zed）：edit 仅 owner/delegate/tenant->ops，不经 customer 层级传播
	if check(t, c, PermEdit, ResOrder, "ORD-2001", "alice-sales") {
		t.Error("edit must not propagate via customer owner (schema.zed edit = owner + delegate + tenant->ops)")
	}
	// 客户 account_owner 变更 = 订单可见范围即时变更
	writeRel(t, c, ResCustomer, "cust-0001", "account_owner", "hannah-new-sales")
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResCustomer, ResourceID: "cust-0001", Relation: "account_owner", SubjectType: ResUser, SubjectID: "alice-sales"}); err != nil {
		t.Fatalf("delete customer account_owner: %v", err)
	}
	if check(t, c, PermView, ResOrder, "ORD-2001", "alice-sales") {
		t.Error("old owner must lose visibility after customer ownership transfer")
	}
	if !check(t, c, PermView, ResOrder, "ORD-2001", "hannah-new-sales") {
		t.Error("new owner must see order")
	}
}

// L1-09 · caveat 时效：代办授权到期即失效、续期生效（CQ-13）
func TestL109CaveatExpiry(t *testing.T) {
	c := startTestServer(t)
	seedOrderGraph(t, c, "ORD-3001")
	ctx := context.Background()
	const order = "ORD-3001"

	// 到期即失效（now 注入判定）
	soon := time.Now().Add(2 * time.Second)
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: order, Relation: "delegate", SubjectType: ResUser, SubjectID: "d1"}, soon); err != nil {
		t.Fatalf("write delegate: %v", err)
	}
	now := time.Now()
	if !checkAt(t, c, PermView, ResOrder, order, "d1", &now) {
		t.Error("delegate should be active before expiry")
	}
	time.Sleep(3 * time.Second)
	later := time.Now()
	if checkAt(t, c, PermView, ResOrder, order, "d1", &later) {
		t.Error("delegate must lose access after expiry (caveat expiry_ok)")
	}

	// 续期生效：重写为未来过期时间，立即判定（now = 当前时刻）
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResOrder, ResourceID: order, Relation: "delegate", SubjectType: ResUser, SubjectID: "d1"}, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("renew delegate: %v", err)
	}
	now2 := time.Now()
	if !checkAt(t, c, PermView, ResOrder, order, "d1", &now2) {
		t.Error("renewed delegate should regain access")
	}
}

// 补充集成：非法 schema → ApplySchema 报错（编译门禁反向）
func TestL107bInvalidSchemaRejected(t *testing.T) {
	c := startTestServer(t)
	err := c.ApplySchema(context.Background(), "this is not a valid schema {")
	if err == nil {
		t.Fatal("invalid schema should be rejected")
	}
}

// ── rebac_cases_spicedb.html 场景 A–H 对象级权限矩阵（L1-07 扩展；D1/D3 落地验证）──

// seedReBACWorld 播种场景对象图：客户负责人/联系人/备货单/发货单/凭证/租户角色。
func seedReBACWorld(t *testing.T, c *SpiceDBClient) {
	writeRel(t, c, ResTenant, "chang-an", "ops", "dave-op")
	writeRel(t, c, ResTenant, "chang-an", "accounting", "erin-acct")
	writeRel(t, c, ResTenant, "chang-an", "admin", "grace-admin")
	writeRel(t, c, ResTenant, "chang-an", "member", "zhang-ming")
	writeRel(t, c, ResTenant, "zhichu", "member", "z-user") // 跨租户（场景 E）
	// 场景 A/H：客户负责人与联系人
	writeRel(t, c, ResCustomer, "tianhua", "account_owner", "zhang-ming")
	// 场景 A：订单层级边（create_prestock 聚合入口经 customer/tenant）
	writeSubjectRel(t, c, ResOrder, "ORD-A1", "customer", ResCustomer, "tianhua")
	writeSubjectRel(t, c, ResOrder, "ORD-A1", "tenant", ResTenant, "chang-an")
	writeRel(t, c, ResCustomer, "tianhua", "contact", "li-customer")
	// 场景 B：备货单负责人
	writeRel(t, c, ResPrestock, "PS-8801", "owner", "zhang-ming")
	writeSubjectRel(t, c, ResPrestock, "PS-8801", "customer", ResCustomer, "tianhua")
	writeSubjectRel(t, c, ResPrestock, "PS-8801", "tenant", ResTenant, "chang-an")
	// 场景 C：发货单仓管 + 场景 B/F：发起人
	writeRel(t, c, ResShipment, "SHP-02", "warehouse_operator", "wang-qiang")
	writeRel(t, c, ResShipment, "SHP-02", "requester", "zhang-ming")
	writeSubjectRel(t, c, ResShipment, "SHP-02", "tenant", ResTenant, "chang-an")
	// 场景 D：客户联系人物流视图 + 凭证上传者
	writeRel(t, c, ResShipment, "SHP-02", "customer_contact", "li-customer")
	writeRel(t, c, ResEvidence, "EVD-1", "uploader", "li-customer")
	writeSubjectRel(t, c, ResEvidence, "EVD-1", "tenant", ResTenant, "chang-an")
}

// TestL107bReBACEightScenarios · rebac 8 场景对象级权限矩阵（CQ-01/02/15）
func TestL107bReBACEightScenarios(t *testing.T) {
	c := startTestServer(t)
	seedReBACWorld(t, c)

	// 场景 A：开备货单（customer.account_owner + 商务）
	if !check(t, c, PermCreatePrestock, ResOrder, "ORD-A1", "zhang-ming") {
		t.Error("A: account_owner should create_prestock")
	}
	if !check(t, c, PermCreatePrestock, ResOrder, "ORD-A1", "dave-op") {
		t.Error("A: tenant.ops should create_prestock")
	}
	if check(t, c, PermCreatePrestock, ResOrder, "ORD-A1", "z-user") {
		t.Error("A: cross-tenant must not create_prestock (scenario E)")
	}

	// 场景 B：备货单负责人改额度/发货/关闭
	for _, perm := range []Permission{PermChangeQuota, PermRequestShipment, PermClose} {
		if !check(t, c, perm, ResPrestock, "PS-8801", "zhang-ming") {
			t.Errorf("B: prestock owner should %s", perm)
		}
	}
	if check(t, c, PermChangeQuota, ResPrestock, "PS-8801", "wang-qiang") {
		t.Error("B: non-owner must not change_quota")
	}
	// 客户负责人经 customer 层级边可 view 备货单
	if !check(t, c, PermView, ResPrestock, "PS-8801", "zhang-ming") {
		t.Error("B: owner should view prestock")
	}

	// 场景 C：仓管确认/驳回
	if !check(t, c, PermConfirm, ResShipment, "SHP-02", "wang-qiang") {
		t.Error("C: warehouse_operator should confirm")
	}
	if !check(t, c, PermReject, ResShipment, "SHP-02", "wang-qiang") {
		t.Error("C: warehouse_operator should reject")
	}
	if check(t, c, PermConfirm, ResShipment, "SHP-02", "zhang-ming") {
		t.Error("C: requester must not confirm (only warehouse_operator)")
	}

	// 场景 D：客户联系人物流视图 + 凭证上传/查看
	if !check(t, c, PermView, ResShipment, "SHP-02", "li-customer") {
		t.Error("D: customer_contact should view shipment logistics")
	}
	if !check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "li-customer") {
		t.Error("D: customer_contact should append_evidence")
	}
	if check(t, c, PermView, ResShipment, "SHP-02", "z-user") {
		t.Error("D: unrelated user must not view shipment")
	}
	if !check(t, c, PermView, ResEvidence, "EVD-1", "li-customer") {
		t.Error("D: uploader should view own evidence")
	}
	if !check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "wang-qiang") {
		t.Error("D: warehouse_operator should append_evidence")
	}
	// AUTH-02（v0.1）：A2 主路径=财务后台上传、v0.1 商务承接 INV，二者必须可追加凭证
	if !check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "erin-acct") {
		t.Error("A2: tenant.accounting should append_evidence (财务主路径)")
	}
	if !check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "dave-op") {
		t.Error("v0.1: tenant.ops should append_evidence (商务承接)")
	}
	if check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "z-user") {
		t.Error("D: unrelated user must not append_evidence")
	}

	// 场景 F：作废 = requester（阶段二窗口由 workflow/caveat 判定）
	if !check(t, c, PermVoid, ResShipment, "SHP-02", "zhang-ming") {
		t.Error("F: requester should void")
	}
	if check(t, c, PermVoid, ResShipment, "SHP-02", "wang-qiang") {
		t.Error("F: warehouse_operator must not void")
	}

	// 场景 E：跨租户全拒绝
	if check(t, c, PermView, ResPrestock, "PS-8801", "z-user") {
		t.Error("E: cross-tenant must not view prestock")
	}
	if check(t, c, PermView, ResEvidence, "EVD-1", "z-user") {
		t.Error("E: cross-tenant must not view evidence")
	}
}

// TestL107cDelegateAndContactRotation · 场景 G 临时代办 + 场景 H 换对接人（CQ-13/CQ-02）
func TestL107cDelegateAndContactRotation(t *testing.T) {
	c := startTestServer(t)
	seedReBACWorld(t, c)
	ctx := context.Background()

	// 场景 G：临时代办（caveat expiry_ok 有效期；TTL 到期由 Outbox 删元组）
	now := time.Now()
	future := now.Add(72 * time.Hour) // 3 天代办
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResPrestock, ResourceID: "PS-8801", Relation: "delegate", SubjectType: ResUser, SubjectID: "li-na"}, future); err != nil {
		t.Fatalf("write delegate: %v", err)
	}
	if !checkAt(t, c, PermChangeQuota, ResPrestock, "PS-8801", "li-na", &now) {
		t.Error("G: in-term delegate should change_quota")
	}
	past := now.Add(-time.Hour)
	if err := c.WriteCaveatedRelationship(ctx, Relationship{ResourceType: ResPrestock, ResourceID: "PS-8801", Relation: "delegate", SubjectType: ResUser, SubjectID: "li-expired"}, past); err != nil {
		t.Fatalf("write expired delegate: %v", err)
	}
	if checkAt(t, c, PermChangeQuota, ResPrestock, "PS-8801", "li-expired", &now) {
		t.Error("G: expired delegate must lose access")
	}
	// 元组删除 = 权限即时收回（场景 G 的 Outbox Delete 语义）
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResPrestock, ResourceID: "PS-8801", Relation: "delegate", SubjectType: ResUser, SubjectID: "li-na"}); err != nil {
		t.Fatalf("delete delegate: %v", err)
	}
	if checkAt(t, c, PermChangeQuota, ResPrestock, "PS-8801", "li-na", &now) {
		t.Error("G: deleted delegate tuple must lose access immediately")
	}

	// 场景 H：客户换对接人（Delete 旧 + Write 新 → 权限自动联动）
	writeRel(t, c, ResShipment, "SHP-02", "customer_contact", "wang-customer")
	if err := c.DeleteRelationship(ctx, Relationship{ResourceType: ResShipment, ResourceID: "SHP-02", Relation: "customer_contact", SubjectType: ResUser, SubjectID: "li-customer"}); err != nil {
		t.Fatalf("delete old contact: %v", err)
	}
	if check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "li-customer") {
		t.Error("H: old contact must lose access after rotation")
	}
	if !check(t, c, PermAppendEvidence, ResShipment, "SHP-02", "wang-customer") {
		t.Error("H: new contact should gain same access")
	}
}

// ── L1-15 · Outbox 投影全链（真 PG 业务事务 → 投影器 → SpiceDB 判定；决策 D4）──
// 覆盖场景 A/B/C/D/G/H 的关系元组自动同步。

func TestL115OutboxProjectionFullChain(t *testing.T) {
	c := startTestServer(t)
	ctx := context.Background()

	// 真 PG（L1-15 需要业务事务；用独立库避免与 repo 包集成测试的 schema 重置冲突）
	dsn := os.Getenv("PILOT_REBAC_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:55432/b2b_pilot_rebac_test?sslmode=disable"
	}
	r, err := repo.New(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable (%v) — L1-15 skipped (degraded mode)", err)
	}
	defer r.Close()
	// 重置 schema + 迁移（含 004_outbox.sql）
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	if err := r.SeedDefaultWorld(ctx, "l1-15"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := r.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("inventory: %v", err)
	}

	projector := NewProjector(r, c)

	// 建单 + 状态推进
	key, _ := domain.OrderKey("chang-an", "quick", "l115")
	ord, _, err := r.CreateOrder(ctx, repo.NewOrder{
		TenantID: "chang-an", CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A",
		IdempotencyKey: key, Payload: []byte(`{"l1":"15"}`),
		Items: []repo.NewOrderItem{{
			ID: "item-l115", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 100, UnitPriceCents: 1000,
		}},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	for _, st := range [][2]string{
		{domain.StateORDRequested, domain.StateORDPriceReviewing},
		{domain.StateORDPriceReviewing, domain.StateORDPriceApproved},
		{domain.StateORDPriceApproved, domain.StateORDContractGenerate},
		{domain.StateORDContractGenerate, domain.StateORDContracted},
		{domain.StateORDContracted, domain.StateSTKReadyForStock},
		{domain.StateSTKReadyForStock, domain.StateSTKStockReviewing},
	} {
		if err := r.UpdateOrderStateTx(ctx, "chang-an", ord.ID, st[0], st[1], "x", "u", "r", nil); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	// 场景 A/B：备货单创建 → 投影 → prestock#owner 生效
	if err := r.ReserveStockTx(ctx, repo.StockPrepare{
		ID: "STK-l115", TenantID: "chang-an", OrderID: ord.ID, WarehouseID: "wh-yangyuan",
		State: domain.StateSTKWarehouseConfirm, PlannedQty: 100, RemainingQty: 100,
		BatchSeq: 1, IdempotencyKey: "sp-l115",
	}, []repo.StockPrepareItem{{
		ID: "spi-l115", TenantID: "chang-an", StockPrepareID: "STK-l115",
		OrderItemID: "item-l115", ProductID: "prod-c68", Qty: 100,
	}}, domain.StateSTKStockReviewing, domain.StateSTKWarehouseConfirm, "u-op", "商务备货", nil); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project: %v", err)
	}
	if !check(t, c, PermView, ResPrestock, "STK-l115", "u-op") {
		t.Fatal("L1-15: prestock#owner should view after projection (scenario A/B)")
	}

	// 场景 C：发货单创建 → 投影 → warehouse_operator 确认
	if err := r.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKWarehouseConfirm,
		domain.StateSTKShipOrderIssued, "x", "u", "r", nil); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := r.UpdateOrderStateTx(ctx, "chang-an", ord.ID, domain.StateSTKShipOrderIssued,
		domain.StateSHPShipAssigning, "x", "u", "r", nil); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := r.ShipAndUpdateOrderTx(ctx, repo.Shipment{
		ID: "SHP-l115", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: "STK-l115",
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 100, BatchSeq: 1, IdempotencyKey: "ship-l115",
	}, []repo.ShipmentItem{{
		ID: "si-l115", TenantID: "chang-an", ShipmentID: "SHP-l115",
		OrderItemID: "item-l115", ProductID: "prod-c68", Qty: 100,
	}}, domain.StateSHPShipAssigning, domain.StateSHPShipPed, "u-wh-yangyuan", "实发", nil); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project2: %v", err)
	}
	if !check(t, c, PermConfirm, ResShipment, "SHP-l115", "u-wh-yangyuan") {
		t.Fatal("L1-15: shipment#warehouse_operator should confirm (scenario C)")
	}
	if !check(t, c, PermView, ResShipment, "SHP-l115", "u-op") {
		t.Fatal("L1-15: shipment#requester should view (scenario B)")
	}

	// 场景 H：客户联系人变更 → 投影 → 旧失效新生效
	if _, err := r.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "li-customer"); err != nil {
		t.Fatalf("set contact: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project3: %v", err)
	}
	if !check(t, c, PermView, ResCustomer, "cust-0001", "li-customer") {
		t.Fatal("L1-15: customer#contact should view (scenario D/H)")
	}
	if _, err := r.SetCustomerContactTx(ctx, "chang-an", "cust-0001", "wang-customer"); err != nil {
		t.Fatalf("rotate contact: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project4: %v", err)
	}
	if check(t, c, PermView, ResCustomer, "cust-0001", "li-customer") {
		t.Fatal("L1-15: old contact must lose access after rotation (scenario H)")
	}
	if !check(t, c, PermView, ResCustomer, "cust-0001", "wang-customer") {
		t.Fatal("L1-15: new contact should gain access (scenario H)")
	}

	// 场景 G：临时代办 grant → 投影 → delegate 生效（caveat now 注入）
	exp := time.Now().Add(72 * time.Hour)
	if err := r.GrantDelegationTx(ctx, "chang-an", "prestock", "STK-l115", "li-na", exp); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project5: %v", err)
	}
	now := time.Now()
	if !checkAt(t, c, PermChangeQuota, ResPrestock, "STK-l115", "li-na", &now) {
		t.Fatal("L1-15: granted delegate should change_quota (scenario G)")
	}
	// revoke → 投影 → 即时失效
	if err := r.RevokeDelegationTx(ctx, "chang-an", "prestock", "STK-l115", "li-na"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := projector.RunOnce(ctx, 100); err != nil {
		t.Fatalf("project6: %v", err)
	}
	if checkAt(t, c, PermChangeQuota, ResPrestock, "STK-l115", "li-na", &now) {
		t.Fatal("L1-15: revoked delegate must lose access (scenario G)")
	}
}

func strPtr(s string) *string { return &s }
