//go:build integration

package repo

import (
	"context"
	"testing"

	"b2b_pilot_v3/internal/domain"
)

// ── 补充集成测试：错误路径/边界（覆盖率补强；倒推影响面同主矩阵）──

// 查询错误路径：not found / 空结果
func TestRepoErrorPaths(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// GetOrder 不存在
	if _, err := e.repo.GetOrder(ctx, "chang-an", "ORD-nope"); !IsNotFound(err) {
		t.Fatalf("GetOrder unknown: %v", err)
	}
	// GetOrderItems 空
	items, err := e.repo.GetOrderItems(ctx, "chang-an", "ORD-nope")
	if err != nil || len(items) != 0 {
		t.Fatalf("GetOrderItems unknown: %v %v", items, err)
	}
	// GetCustomerGrade 不存在
	if _, err := e.repo.GetCustomerGrade(ctx, "chang-an", "cust-nope"); !IsNotFound(err) {
		t.Fatalf("GetCustomerGrade unknown: %v", err)
	}
	// GetPreOrder 不存在 / GetPreOrderByMsg 不存在
	if _, err := e.repo.GetPreOrder(ctx, "chang-an", "PRE-nope"); !IsNotFound(err) {
		t.Fatalf("GetPreOrder unknown: %v", err)
	}
	if _, err := e.repo.GetPreOrderByMsg(ctx, "chang-an", "g", "m"); !IsNotFound(err) {
		t.Fatalf("GetPreOrderByMsg unknown: %v", err)
	}
	// GetApprovalTask 不存在
	if _, err := e.repo.GetApprovalTask(ctx, "chang-an", "AT-nope"); !IsNotFound(err) {
		t.Fatalf("GetApprovalTask unknown: %v", err)
	}
	// GetShipment / GetInvoice 不存在
	if _, err := e.repo.GetShipment(ctx, "chang-an", "SHP-nope"); !IsNotFound(err) {
		t.Fatalf("GetShipment unknown: %v", err)
	}
	if _, err := e.repo.GetInvoice(ctx, "chang-an", "INV-nope"); !IsNotFound(err) {
		t.Fatalf("GetInvoice unknown: %v", err)
	}
	// GetInventory 不存在
	if _, err := e.repo.GetInventory(ctx, "chang-an", "wh-x", "prod-x"); !IsNotFound(err) {
		t.Fatalf("GetInventory unknown: %v", err)
	}
	// 未知租户游标 = 0
	seq, err := e.repo.GetArchiveCursor(ctx, "zhichu")
	if err != nil || seq != 0 {
		t.Fatalf("cursor unknown tenant: %d %v", seq, err)
	}
	// ListOrders 空租户
	orders, err := e.repo.ListOrders(ctx, "zhichu", "", 10)
	if err != nil || len(orders) != 0 {
		t.Fatalf("ListOrders empty: %v %v", orders, err)
	}
	// ListPreOrders 空
	pres, err := e.repo.ListPreOrders(ctx, "chang-an", 10)
	if err != nil || len(pres) != 0 {
		t.Fatalf("ListPreOrders empty: %v %v", pres, err)
	}
}

// 幂等键冲突与状态机错误路径
func TestRepoConflictPaths(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	// 预订单：同键不同消息 → 报错（消息幂等键冲突不产生新单）
	key, _ := domain.PreOrderKey("chang-an", "g1", "m1", "v1")
	po, created, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: key,
	})
	if err != nil || !created {
		t.Fatalf("create preorder: %v %v", created, err)
	}
	// 同 (tenant, group, msgid) 同键 → 幂等返回既有
	po2, created2, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m1", Candidates: []byte(`[]`), IdempotencyKey: key,
	})
	if err != nil || created2 || po2.ID != po.ID {
		t.Fatalf("preorder idempotent replay: %v %v", created2, err)
	}

	// 状态迁移：from 不匹配 → not found
	ord := e.createTestOrder("款到", 10)
	tx, err := e.repo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := e.repo.UpdateOrderState(ctx, tx, "chang-an", ord.ID, "BOGUS_STATE",
		domain.StateORDPriceReviewing, "x", "u", "r", nil); !IsNotFound(err) {
		t.Fatalf("update from wrong state: %v", err)
	}
	// 预订单状态迁移：from 不匹配
	preKey, _ := domain.PreOrderKey("chang-an", "g1", "m2", "v1")
	po3, _, err := e.repo.CreatePreOrder(ctx, NewPreOrder{
		TenantID: "chang-an", GroupID: "g1", MsgID: "m2", Candidates: []byte(`[]`), IdempotencyKey: preKey,
	})
	if err != nil {
		t.Fatalf("create preorder3: %v", err)
	}
	if err := e.repo.UpdatePreOrderState(ctx, tx, "chang-an", po3.ID, "BOGUS",
		domain.StatePrePendingConfirm, "x", "r", nil, ""); !IsNotFound(err) {
		t.Fatalf("preorder from wrong state: %v", err)
	}

	// 审批任务：未知订单 → FK 错误路径（未知任务裁决 covered in L1-11）
	// CloseShipment 非 SHIPPED 状态 → not found
	if err := e.repo.CloseShipment(ctx, tx, "chang-an", "SHP-nope", "u-op"); !IsNotFound(err) {
		t.Fatalf("close unknown shipment: %v", err)
	}
	// SetInventory 负数 → 拒绝
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", -1); err == nil {
		t.Fatal("negative inventory should be rejected")
	}
}

// 元信息事务包装：合同/静默修改/resubmit 链/补偿
func TestRepoMetaTxWrappers(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	ord := e.createTestOrder("款到", 10)
	// SetOrderContractTx
	if err := e.repo.SetOrderContractTx(ctx, "chang-an", ord.ID, "HT-TEST-001"); err != nil {
		t.Fatalf("set contract: %v", err)
	}
	o, _ := e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.ContractNo == nil || *o.ContractNo != "HT-TEST-001" {
		t.Fatal("contract not persisted")
	}
	// UpdateOrderMetaTx（E 类静默修改）
	if err := e.repo.UpdateOrderMetaTx(ctx, "chang-an", ord.ID, "HT-TEST-001", []byte(`{"e":true}`)); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	o, _ = e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if string(o.Payload) != `{"e":true}` && string(o.Payload) != `{"e": true}` {
		t.Fatalf("meta payload: %s", o.Payload)
	}
	// UpdateResubmitChainTx
	if err := e.repo.UpdateResubmitChainTx(ctx, "chang-an", ord.ID, 1, ord.ID); err != nil {
		t.Fatalf("resubmit chain: %v", err)
	}
	o, _ = e.repo.GetOrder(ctx, "chang-an", ord.ID)
	if o.ResubmitRevision != 1 || o.SubmissionMode != "resubmit" || o.ResubmitFromOrderID == nil {
		t.Fatalf("resubmit chain: rev=%d mode=%s", o.ResubmitRevision, o.SubmissionMode)
	}
	// RecordFlowFailed（补偿事件留痕）
	if err := e.repo.RecordFlowFailed(ctx, "chang-an", ord.ID, "inject", nil); err != nil {
		t.Fatalf("record flow failed: %v", err)
	}
	events, err := e.repo.ListOrderEvents(ctx, "chang-an", ord.ID, 100)
	if err != nil || len(events) < 2 {
		t.Fatalf("events after flow failed: %d %v", len(events), err)
	}
	// CountShipments / SumShippedByOrderItem 空
	n, err := e.repo.CountShipments(ctx, "chang-an", ord.ID)
	if err != nil || n != 0 {
		t.Fatalf("count shipments: %d %v", n, err)
	}
	sums, err := e.repo.SumShippedByOrderItem(ctx, "chang-an", ord.ID)
	if err != nil || len(sums) != 0 {
		t.Fatalf("sum shipped: %v %v", sums, err)
	}
}

// 发货/开票幂等重放：同键再次 Apply → 不重复生效
func TestRepoIdempotentReplay(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()
	if err := e.repo.SetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68", 100); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	ord := e.createTestOrder("款到", 100)
	item := e.getOrderItem(ord.ID)
	spID := e.createTestPrepare(ord.ID, item.ID, "wh-yangyuan", 100)

	ship := Shipment{
		ID: "shp-replay", TenantID: "chang-an", OrderID: ord.ID, StockPrepareID: spID,
		WarehouseID: "wh-yangyuan", State: domain.ShipmentStateShipped, PlannedQty: 100,
		ShippedQty: 40, BatchSeq: 1, IdempotencyKey: "ship-replay-key",
	}
	shipItems := []ShipmentItem{{
		ID: "si-replay", TenantID: "chang-an", ShipmentID: "shp-replay",
		OrderItemID: item.ID, ProductID: "prod-c68", Qty: 40,
	}}
	// 第一次：成功
	tx, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx, ship, shipItems, "u-wh-yangyuan"); err != nil {
		t.Fatalf("apply shipment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 0.1 M1：锁库存暂缓——库存不扣减（保持 100）
	inv, _ := e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if inv.Qty != 100 {
		t.Fatalf("0.1 inventory untouched (M1) = %d", inv.Qty)
	}
	// 重放：同键 → 跳过（发货量不重复累计）
	tx2, _ := e.repo.Begin(ctx)
	if err := e.repo.ApplyShipment(ctx, tx2, ship, shipItems, "u-wh-yangyuan"); err != nil {
		t.Fatalf("replay shipment: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	inv, _ = e.repo.GetInventory(ctx, "chang-an", "wh-yangyuan", "prod-c68")
	if inv.Qty != 100 {
		t.Fatalf("inventory after replay = %d (must be unchanged)", inv.Qty)
	}
}
