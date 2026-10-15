// scripts/seedrel —— 授权关系 seed（真实模式部署必需）
//
// 真实 SpiceDB 模式下（B2B_SPICEDB_ADDR 生效），权限关系由 Outbox 投影器
// （阶段二，docs/05 §3）自动维护——本地/演示环境投影器未接线，需本工具
// 预置基线关系（管理角色/客户归属/仓库/订单层级），否则 HTTP API 的
// 授权预检（order#view 等）将拒绝一切请求。
//
// 用法（首次初始化含 schema）：
//
//	go run ./scripts/seedrel -spicedb 127.0.0.1:15051 -token <key> -schema schema.zed
//	go run ./scripts/seedrel -spicedb 127.0.0.1:15051 -token <key> -orders "ORD-xxx:cust-0001"
//
// 幂等：TouchRelationship 语义幂等，可重复执行。
package main

import (
	"context"
	"flag"
	"log"
	"strings"

	"b2b_pilot_v3/internal/authz"
)

func touch(ctx context.Context, c *authz.SpiceDBClient, r authz.Relationship) {
	if err := c.TouchRelationship(ctx, r); err != nil {
		log.Printf("WARN %s:%s#%s@%s: %v", r.ResourceType, r.ResourceID, r.Relation, r.SubjectID, err)
		return
	}
	log.Printf("ok   %s:%s#%s@%s", r.ResourceType, r.ResourceID, r.Relation, r.SubjectID)
}

func main() {
	addr := flag.String("spicedb", "127.0.0.1:50051", "SpiceDB gRPC 地址")
	token := flag.String("token", "b2b-dev-key", "SpiceDB 预共享令牌")
	schemaFile := flag.String("schema", "", "schema.zed 路径；首次初始化（object definition 不存在）时必须传入")
	orders := flag.String("orders", "", "订单关系 seed：逗号分隔 \"orderID:customerID\" 列表（写 order#owner@u-sales + order#customer 层级边；投影器阶段二未接线时的等价物）")
	flag.Parse()

	ctx := context.Background()
	c, err := authz.NewSpiceDB(*addr, *token)
	if err != nil {
		log.Fatalf("connect spicedb: %v", err)
	}
	if *schemaFile != "" {
		if err := c.ApplySchemaFromFile(ctx, *schemaFile); err != nil {
			log.Fatalf("apply schema: %v", err)
		}
		log.Printf("schema applied: %s", *schemaFile)
	}

	// 演示基线关系（chang-an 租户；与 cmd/api demoRoleResolver 的用户集对齐）：
	//   tenant#ops/admin/accounting → 管理层读、财务
	//   customer#account_owner/contact → 场景 A/D/H
	//   warehouse#staff → 仓管发货权
	rels := []authz.Relationship{
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "ops", SubjectID: "u-op"},
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "ops", SubjectID: "u-supervisor"},
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "ops", SubjectID: "bob-super"},
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "admin", SubjectID: "grace-admin"},
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "accounting", SubjectID: "u-accounting"},
		{ResourceType: authz.ResTenant, ResourceID: "chang-an", Relation: "accounting", SubjectID: "erin-acct"},
		{ResourceType: authz.ResCustomer, ResourceID: "cust-0001", Relation: "account_owner", SubjectID: "u-supervisor"},
		{ResourceType: authz.ResCustomer, ResourceID: "cust-0001", Relation: "contact", SubjectID: "u-contact"},
		{ResourceType: authz.ResCustomer, ResourceID: "cust-0002", Relation: "account_owner", SubjectID: "u-supervisor"},
		{ResourceType: authz.ResWarehouse, ResourceID: "wh-yangyuan", Relation: "staff", SubjectID: "u-wh-yangyuan"},
		{ResourceType: authz.ResWarehouse, ResourceID: "wh-yangyuan", Relation: "staff", SubjectID: "frank-wh"},
		{ResourceType: authz.ResWarehouse, ResourceID: "wh-dongguan", Relation: "staff", SubjectID: "u-wh-dongguan"},
	}
	for _, r := range rels {
		touch(ctx, c, r)
	}

	if *orders != "" {
		for _, spec := range strings.Split(*orders, ",") {
			parts := strings.SplitN(spec, ":", 2)
			if len(parts) != 2 {
				log.Fatalf("bad -orders spec %q (want orderID:customerID)", spec)
			}
			touch(ctx, c, authz.Relationship{ResourceType: authz.ResOrder, ResourceID: parts[0], Relation: "owner", SubjectID: "u-sales"})
			touch(ctx, c, authz.Relationship{ResourceType: authz.ResOrder, ResourceID: parts[0], Relation: "customer", SubjectID: parts[1], SubjectType: authz.ResCustomer})
			touch(ctx, c, authz.Relationship{ResourceType: authz.ResOrder, ResourceID: parts[0], Relation: "tenant", SubjectID: "chang-an", SubjectType: authz.ResTenant})
		}
	}
	log.Printf("seedrel 完成（幂等，可重复执行）")
}
