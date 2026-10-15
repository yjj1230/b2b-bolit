//go:build integration

// Package l3sys —— L3 · 系统验证（docs/03-testing.html L3）
//
// 真实全栈驱动：HTTP 全链（LocalRunner + 真 PG）→ L3 脚本（seeddata 幂等 /
// reconcile 四守恒 / audit 审计链 / concurrency 并发守恒）→ 断言退出码。
// 使用独立 DB（B2B_L3_PG_DSN，默认 55432/b2b_pilot），与 L1/L2 的 b2b_pilot_test
// 隔离（避免 schema 重置互相干扰）。
package l3sys

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/tests/testkit"
)

func l3DSN() string {
	if dsn := os.Getenv("B2B_L3_PG_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://postgres:postgres@localhost:55432/b2b_pilot?sslmode=disable"
}

// runScript 运行 L3 脚本并断言退出码 0（输出透传）。
func runScript(t *testing.T, name string, args ...string) {
	t.Helper()
	root, err := filepath.Abs("../../")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	cmd := exec.Command("go", append([]string{"run", "./scripts/" + name}, args...)...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script %s failed: %v", name, err)
	}
}

// TestL3SystemDrivers L3-02/05/06 + HTTP 全链：seed → 全链 → 对账 → 审计 → 幂等重跑。
func TestL3SystemDrivers(t *testing.T) {
	dsn := l3DSN()

	// 独立环境：schema 重置 + 迁移 + 种子世界（同 DB 供脚本后续对账）
	t.Setenv("PILOT_TEST_PG_DSN", dsn)
	e := testkit.NewEnv(t)

	// HTTP 全链驱动一笔订单到 CLOSED（L3 系统级：业务流经 API 真实落库）
	orderID := e.CreateOrder("款到", "order:chang-an:quick:l3-sys-1")
	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, "price-review", `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, "stock-review", `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
	e.Signal(orderID, "warehouse-confirm", `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
	e.Signal(orderID, "payment-confirm", `{"verdict":"APPROVED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, "ship-action", `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`)
	e.WaitOrderState(orderID, domain.StateINVInvoicing)
	e.Signal(orderID, "invoice-issued", `{"verdict":"ISSUED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVPaymentTracking)
	e.Signal(orderID, "payment-tracked", `{"amount":100000,"operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateINVClosed)

	// L3-06：种子数据（runID 注入，重复跑幂等）→ 首跑
	runScript(t, "seeddata", "-dsn", dsn, "-run", "l3-sys")

	// L3-02：全库对账（四守恒聚合；HTTP 订单 + 种子订单全部覆盖）
	runScript(t, "reconcile", "-dsn", dsn)

	// L3-05：审计链回放（断链/合法迁移/终态分录）
	runScript(t, "audit", "-dsn", dsn)

	// L3-06 幂等：重跑 seeddata 不报错，对账仍绿
	runScript(t, "seeddata", "-dsn", dsn, "-run", "l3-sys")
	runScript(t, "reconcile", "-dsn", dsn)

	// L3-01：并发守恒（自种子：8 并发 × 30 ≤ 库存 100 → 3 成功 5 拒绝）
	runScript(t, "concurrency", "-dsn", dsn)

	// 系统级终态抽查：HTTP 订单事件链完整回放
	rec := e.DoJSON("GET", "/api/v1/orders/"+orderID+"/events", "", http.StatusOK, nil)
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	requireNoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	if len(out.Events) < 12 {
		t.Fatalf("event chain too short: %d", len(out.Events))
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestL3HTTPIdempotentReplay 系统级幂等重放：同键下单不重复建单、流程不重复推进。
func TestL3HTTPIdempotentReplay(t *testing.T) {
	dsn := l3DSN()
	t.Setenv("PILOT_TEST_PG_DSN", dsn)
	e := testkit.NewEnv(t)

	body := `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:l3-idem-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`
	rec1 := e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o1 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	requireNoError(t, json.Unmarshal(rec1.Body.Bytes(), &o1))
	if !o1.Created {
		t.Fatal("first create should be created=true")
	}

	// 流程推进后重放同键 → 返回同一订单（不重复启动、不重复推进）
	e.WaitOrderState(o1.OrderID, domain.StateORDPriceReviewing)
	rec2 := e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o2 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	requireNoError(t, json.Unmarshal(rec2.Body.Bytes(), &o2))
	if o2.Created || o2.OrderID != o1.OrderID {
		t.Fatalf("idempotent replay broken: %+v vs %+v", o2, o1)
	}

	// 终态仍可达（重放未破坏流程；0.1：备货选仓 → 仓管确认）
	e.Signal(o1.OrderID, "price-review", `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(o1.OrderID, domain.StateSTKStockReviewing)
	e.Signal(o1.OrderID, "stock-review", `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.OrderState(o1.OrderID) == domain.StateSTKWarehouseConfirm {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := e.OrderState(o1.OrderID); got != domain.StateSTKWarehouseConfirm {
		t.Fatalf("flow broken after replay: %s", got)
	}
}
