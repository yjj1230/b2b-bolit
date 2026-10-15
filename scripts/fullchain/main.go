// scripts/fullchain —— HTTP API 全流程演示（企微接入前的独立验证载体）
//
// 与企微转接层（internal/wecom callback → HTTP API）完全相同的调用面：
// 纯 HTTP 客户端驱动「款到订单全链 → 凭证链 → 预订单链 → 待办/事件」，
// 证明不依赖任何企微通道即可走完整个流程（docs/07 设计约束：企微 = API 之上的转接层）。
//
// 装配与 cmd/api 本地模式一致：真 PG + LocalRunner（进程内真实工作流）+ DemoPolicy
// 虚拟时长 + StaticChecker 对照表 + wecom mock 出站。运行：
//
//	go run ./scripts/fullchain -dsn "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"
//
// 退出码 0 = 全链 PASS；非 0 = 失败（每步打印状态，可作验收证据）。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"b2b_pilot_v3/internal/api"
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

var stepNo int

func step(format string, args ...any) {
	stepNo++
	fmt.Printf("[%02d] %s\n", stepNo, fmt.Sprintf(format, args...))
}

// demoRoleResolver 演示角色解析（与 cmd/api demoRoleOf 对齐；StaticChecker 对照表用）。
func demoRoleResolver(userID string) (authz.Role, error) {
	switch userID {
	case "frank-wh", "u-wh-yangyuan", "u-wh-dongguan":
		return authz.RoleWarehouse, nil
	case "u-accounting", "erin-acct":
		return authz.RoleAccounting, nil
	case "u-chairman", "carol-chair":
		return authz.RoleChairman, nil
	case "u-supervisor", "bob-super":
		return authz.RoleSupervisor, nil
	case "u-admin", "grace-admin":
		return authz.RoleAdmin, nil
	case "dave-sales":
		return authz.RoleSales, nil
	case "u-contact":
		return authz.RoleContact, nil
	}
	return authz.RoleOP, nil
}

// demoRoleOf 角色解析（Server 用；与 demoRoleResolver 同源）。
func demoRoleOf(userID string) (authz.Role, string) {
	r, _ := demoRoleResolver(userID)
	return r, "chang-an"
}

type client struct {
	base   string
	httpc  *http.Client
	sqlDB  *sql.DB
	tenant string
}

func (c *client) do(method, path, body, userID string) (int, []byte) {
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		log.Fatalf("request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		log.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *client) doOK(method, path, body, userID string) []byte {
	code, b := c.do(method, path, body, userID)
	if code < 200 || code >= 300 {
		log.Fatalf("%s %s: status=%d body=%s", method, path, code, b)
	}
	return b
}

func (c *client) state(orderID string) string {
	b := c.doOK("GET", "/api/v1/orders/"+orderID, "", "u-supervisor")
	var out struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		log.Fatalf("parse state: %v (%s)", err, b)
	}
	return out.State
}

func (c *client) waitState(orderID, want string) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.state(orderID); got == want {
			step("state %s = %s", orderID, got)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("order %s: state != %s (last=%s)", orderID, want, c.state(orderID))
}

func (c *client) signal(orderID, name, payload, userID string) {
	step("signal %s → %s", orderID, name)
	c.doOK("POST", fmt.Sprintf("/api/v1/orders/%s/signal/%s", orderID, name), payload, userID)
}

func main() {
	dsn := flag.String("dsn", "postgres://postgres:postgres@localhost:15432/b2b_pilot?sslmode=disable", "PG DSN")
	addr := flag.String("addr", "127.0.0.1:18089", "demo HTTP listen address")
	flag.Parse()

	ctx := context.Background()
	store, err := repo.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer store.Close()

	gw := wecom.NewMockGateway()
	notifier := wecom.NewNotifier(gw)
	checker := authz.NewStaticChecker(demoRoleResolver)
	// 与 tests/l2_e2e 同构：Static 策略（24h/48h/5d 真实时长）。TestSuite 虚拟时钟
	// 会自动触发 Timer，信号抢先到达闸门即走人工分支——E2E 已验证的稳定纪律。
	lr := api.NewLocalRunnerWithPolicyAndData(store, orderdata.NewDemoMemoryProvider(), notifier, checker, integration.NewStaticConfigProvider())
	lr.SetLogger(func(string, ...any) {})
	starter := lr

	srv := api.NewServer(store, starter, checker, demoRoleOf)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	time.Sleep(200 * time.Millisecond) // 等监听就绪

	// 独立 SQL 连接：查发货单 ID（API 投影不暴露 shipment_id 明细）
	sqlDB, err := sql.Open("pgx", *dsn)
	if err != nil {
		log.Fatalf("open sql: %v", err)
	}
	defer sqlDB.Close()

	c := &client{base: "http://" + *addr, httpc: &http.Client{Timeout: 10 * time.Second}, sqlDB: sqlDB}

	step("healthz")
	c.doOK("GET", "/healthz", "", "")

	// ── ① 款到订单全链（CQ-01：人工闸门不可绕过；6 信号 → INV_CLOSED）──
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	key, err := domain.OrderKey("chang-an", "quick", "fullchain-demo-"+runID)
	if err != nil {
		log.Fatalf("order key: %v", err)
	}
	itemID := "item-run-" + runID
	b := c.doOK("POST", "/api/v1/orders", fmt.Sprintf(`{
		"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":%q,
		"items":[{"id":%q,"product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`, key, itemID), "u-sales")
	var ord struct {
		OrderID string `json:"order_id"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(b, &ord); err != nil {
		log.Fatalf("parse create: %v (%s)", err, b)
	}
	orderID := ord.OrderID
	step("created order %s (state=%s)", orderID, ord.State)

	c.waitState(orderID, domain.StateORDPriceReviewing)
	c.signal(orderID, "price-review", `{"verdict":"APPROVED","operator":"u-supervisor"}`, "u-supervisor")
	// 0.1 N4：商务备货闸门（选备货仓 + 收货地址）
	c.waitState(orderID, domain.StateSTKStockReviewing)
	c.signal(orderID, "stock-review", `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`, "u-op")
	c.waitState(orderID, domain.StateSTKWarehouseConfirm)
	c.signal(orderID, "warehouse-confirm", `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`, "u-wh-yangyuan")
	c.waitState(orderID, domain.StateSTKPaymentConfirming)
	c.signal(orderID, "payment-confirm", `{"verdict":"APPROVED","operator":"u-accounting"}`, "u-accounting")
	c.waitState(orderID, domain.StateSHPShipAssigning)
	c.signal(orderID, "ship-action", `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`, "u-wh-yangyuan")
	c.waitState(orderID, domain.StateINVInvoicing)
	c.signal(orderID, "invoice-issued", `{"verdict":"ISSUED","operator":"u-accounting"}`, "u-accounting")
	c.waitState(orderID, domain.StateINVPaymentTracking)
	c.signal(orderID, "payment-tracked", `{"amount":100000,"operator":"u-accounting"}`, "u-accounting")
	c.waitState(orderID, domain.StateINVClosed)

	// 事件链（CQ-12：分域事件审计回放）
	b = c.doOK("GET", "/api/v1/orders/"+orderID+"/events", "", "u-supervisor")
	var ev struct {
		Events []repo.Event `json:"events"`
	}
	if err := json.Unmarshal(b, &ev); err != nil {
		log.Fatalf("parse events: %v", err)
	}
	if len(ev.Events) < 26 {
		log.Fatalf("event chain: got %d < 26", len(ev.Events))
	}
	step("event chain: %d events (≥26)", len(ev.Events))

	// 待办检索（CQ-05）
	code, _ := c.do("GET", "/api/v1/todos?kind=PAYMENT", "", "u-accounting")
	if code != http.StatusOK {
		log.Fatalf("todos: status=%d", code)
	}
	step("todos?kind=PAYMENT → 200")

	// ── ② 凭证链（CQ-15：只追加版本 + 场景 D 权限边界）──
	var shipmentID string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM shipments WHERE tenant_id='chang-an' AND order_id=$1 LIMIT 1`, orderID).Scan(&shipmentID); err != nil {
		log.Fatalf("shipment id: %v", err)
	}
	step("shipment %s (upload evidence)", shipmentID)
	epath := fmt.Sprintf("/api/v1/orders/%s/shipments/%s/evidence", orderID, shipmentID)
	c.doOK("POST", epath, `{"file_url":"s3://demo/v1.png","file_hash":"h1"}`, "u-wh-yangyuan")
	c.doOK("POST", epath, `{"file_url":"s3://demo/v2.png","file_hash":"h2"}`, "u-wh-yangyuan")
	code, b = c.do("POST", epath, `{"file_url":"s3://demo/v3.png","file_hash":"h3"}`, "u-contact")
	if code != http.StatusForbidden {
		log.Fatalf("contact upload: want 403 got %d", code)
	}
	step("evidence v1/v2 created; contact upload → 403 (场景 D)")

	// ── ③ 预订单链（人工确认 → 正式订单）──
	b = c.doOK("POST", "/api/v1/preorders", fmt.Sprintf(`{"group_id":"group-demo","msg_id":"demo-msg-%s","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}],
		"idempotency_key":"pre:chang-an:group-demo:demo-msg-%s:v1"}`, runID, runID), "u-supervisor")
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(b, &pre); err != nil {
		log.Fatalf("parse preorder: %v (%s)", err, b)
	}
	step("preorder %s (state=%s)", pre.PreOrderID, pre.State)
	// 预订单确认（人工闸门：确认前无正式订单）。Demo 策略虚拟时钟会自动推进升级
	// Timer（阻塞即触发），故立即投递确认信号抢占闸门——与 tests/l2_e2e 同一纪律。
	c.doOK("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/preorder-confirm", pre.PreOrderID),
		`{"confirmed":true,"operator":"dave-sales"}`, "dave-sales")
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, b = c.do("GET", "/api/v1/preorders/"+pre.PreOrderID, "", "u-supervisor")
		if code != http.StatusOK {
			log.Fatalf("get preorder: %d", code)
		}
		var p struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(b, &p)
		if p.State == "PRE_ORDER_REQUESTED" {
			step("preorder %s → PRE_ORDER_REQUESTED（已派生正式订单）", pre.PreOrderID)
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("preorder not requested: %s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// ── 出站通知留痕（企微 mock 通道：审批/预订单/状态卡片）──
	step("wecom outbound cards: %d sent (mock)", len(gw.Sent()))
	if len(gw.Sent()) == 0 {
		log.Fatal("expected outbound wecom cards")
	}

	fmt.Printf("\n[fullchain] ALL GREEN — HTTP API 全流程走通（订单全链 INV_CLOSED + 凭证链 + 预订单链 + 待办/事件 + 出站通知）\n")
	_ = os.Stdout.Sync()
}
