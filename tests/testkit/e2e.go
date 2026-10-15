// Package testkit —— tests/ 各 L 分层套件的共享装配器（真实 PG + LocalRunner）。
//
// 目标（docs/03-testing.html L 分层 + 用户要求「HTTP API 业务流全部跑起来」）：
//   - 以真实 PostgreSQL 为底座（PILOT_TEST_PG_DSN，默认 localhost:55432/b2b_pilot_test）
//   - HTTP API 用 api.NewServer 真装配（同 cmd/api local 模式）
//   - 工作流用 api.LocalRunner 进程内执行真实 OrderWorkflow/PreOrderWorkflow
//     （Temporal TestSuite 环境，无 Temporal 服务依赖；02 §2.1 本地模式）
//   - 授权用 authz.StaticChecker（对照表替身；L0-09），出站通知用 wecom mock 网关
//
// 依赖方向：testkit → internal/*（与 cmd/ 同属本模块，符合 internal 可见性规则）。
package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"b2b_pilot_v3/internal/api"
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

// DefaultTestDSN 与 Makefile TEST_PG_DSN 一致。
const DefaultTestDSN = "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"

// Env 一套完整 HTTP API 测试环境（每个测试独立：schema 重置 + 迁移 + 种子世界）。
type Env struct {
	T      *testing.T
	Store  *repo.Repo
	Server *api.Server
	LR     *api.LocalRunner
	DSN    string
	conn   *pgx.Conn
	// GroupGateway 客户群出站 mock（0.1 守门员 N-01 发送时机断言）
	GroupGateway *wecom.MockGateway
}

// Conn 返回底层连接（直接 DB 断言用，如守恒/幂等键/事件链）。
func (e *Env) Conn() *pgx.Conn { return e.conn }

// Options 环境装配选项。
type Options struct {
	// Policy 业务配置 Provider；nil = 本地演示策略（integration.NewDemoPolicy，10s
	// 虚拟时长：TestSuite 自动推进 Timer，短时长避免烧穿 10y 运行预算——见
	// integration/demo.go 说明）。精确时长语义由 internal/workflows TestSuite 覆盖。
	Policy integration.BusinessConfigProvider
	// Starter 工作流启动器；nil = LocalRunner（进程内真实执行）。
	// 组件级套件（L1）传 no-op 替身，避免后台工作流空转。
	Starter api.Starter
}

// NewEnv 装配测试环境。PG 不可用时 t.Skip（与 internal/repo L1 降级语义一致）。
func NewEnv(t *testing.T) *Env { return NewEnvWithOptions(t, Options{}) }

// NewEnvWithOptions 同 NewEnv，可覆盖业务配置（Timer 策略）。
func NewEnvWithOptions(t *testing.T, opts Options) *Env {
	t.Helper()
	dsn := os.Getenv("PILOT_TEST_PG_DSN")
	if dsn == "" {
		dsn = DefaultTestDSN
	}
	ctx := context.Background()
	store, err := repo.New(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable (%v) — integration suite skipped (degraded mode)", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	e := &Env{T: t, Store: store, DSN: dsn, conn: conn}
	e.resetSchema(t)
	e.seedWorld(t)

	// 出站通知：wecom mock 网关（07 §6 wrapper 隔离点；零外部依赖）
	notifier := wecom.NewNotifier(wecom.NewMockGateway())
	// 授权判定：StaticChecker（对照表替身；SpiceDB 集成见 internal/authz L1-07..10）
	checker := authz.NewStaticChecker(demoRoleResolver)
	roleOf := demoRoleOf
	policy := opts.Policy
	if policy == nil {
		policy = integration.NewDemoPolicy()
	}
	starter := opts.Starter
	if starter == nil {
		lr := api.NewLocalRunnerWithPolicyAndData(store, orderdata.NewDemoMemoryProvider(), notifier, checker, policy)
		lr.SetLogger(func(format string, args ...any) { t.Logf("[local-runner] "+format, args...) })
		starter = lr
	}
	server := api.NewServer(store, starter, checker, roleOf)
	// 0.1 守门员：客户群出站 mock（确认后才发送；N-01 断言发送时机）
	groupGW := wecom.NewMockGateway()
	server.GroupSender = groupGW
	// S1 侧边栏 H5 服务端能力（07 §6.1；与 cmd/api 本地模式同构装配）
	server.JSAPI = wecom.NewJSAPIConfigBuilder("ww-chang-an-demo", "1000002", "b2b-demo-jsapi-secret")
	server.Identity = wecom.NewMockIdentityProvider("chang-an", map[string]string{
		"mock-supervisor": "u-supervisor",
		"mock-warehouse":  "u-wh-yangyuan",
		"mock-accounting": "u-accounting",
		"mock-contact":    "u-contact",
	}, map[string]string{
		"chat-cust-0001": "cust-0001",
		"chat-cust-0002": "cust-0002",
	})
	server.Context = wecom.NewContextIssuer("b2b-test-context-secret", 2*time.Hour)

	e.Server = server
	e.GroupGateway = groupGW
	if opts.Starter == nil {
		e.LR = starter.(*api.LocalRunner)
	}
	t.Cleanup(e.close)
	return e
}

// NoopStarter L1 组件级套件用：仅记录启动/信号，不执行工作流。
type NoopStarter struct {
	Started []string
	Signals []string
}

func (n *NoopStarter) StartOrderWorkflow(ctx context.Context, tenantID, orderID string) (string, error) {
	n.Started = append(n.Started, orderID)
	return "noop-" + orderID, nil
}

func (n *NoopStarter) StartPreOrderWorkflow(ctx context.Context, tenantID, preOrderID, senderID string) (string, error) {
	n.Started = append(n.Started, preOrderID)
	return "noop-" + preOrderID, nil
}

func (n *NoopStarter) SignalWorkflow(ctx context.Context, workflowID, signalName string, payload any) error {
	n.Signals = append(n.Signals, signalName)
	return nil
}

// resetSchema 重置 schema 后按序应用全部迁移（与 internal/repo testenv 同语义）。
func (e *Env) resetSchema(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	migrations, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, f := range migrations {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := e.conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

// seedWorld 播种标准演示世界（与 scripts/seeddata 同源）。
func (e *Env) seedWorld(t *testing.T) {
	t.Helper()
	if err := e.Store.SeedDefaultWorld(context.Background(), "testkit-"+fmt.Sprint(time.Now().UnixNano())); err != nil {
		t.Fatalf("seed world: %v", err)
	}
}

func (e *Env) close() {
	// 等待遗留本地工作流结束（防跨用例 schema 重置干扰）；超时仅告警不阻塞
	if e.LR != nil {
		e.LR.WaitAll(5 * time.Second)
	}
	if e.conn != nil {
		_ = e.conn.Close(context.Background())
	}
	if e.Store != nil {
		e.Store.Close()
	}
}

// Do 发起 HTTP 请求（返回 recorder；headers 追加到默认 X-User-Id）。
func (e *Env) Do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	e.T.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("X-User-Id") == "" {
		req.Header.Set("X-User-Id", "u-op")
	}
	rec := httptest.NewRecorder()
	e.Server.Routes().ServeHTTP(rec, req)
	return rec
}

// DoJSON 同 Do，并断言 HTTP 状态码。
func (e *Env) DoJSON(method, path, body string, wantStatus int, headers map[string]string) *httptest.ResponseRecorder {
	e.T.Helper()
	rec := e.Do(method, path, body, headers)
	if rec.Code != wantStatus {
		e.T.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

// CreateOrder 快速下单（默认款到/100 件/wh-yangyuan/prod-c68），返回订单 ID。
func (e *Env) CreateOrder(paymentTerm, idemKey string) string {
	return e.CreateOrderWithItem(paymentTerm, idemKey, "item-1")
}

// CreateOrderWithItem 同 CreateOrder，可指定明细 ID（一测试多订单避免 order_items PK 冲突）。
func (e *Env) CreateOrderWithItem(paymentTerm, idemKey, itemID string) string {
	e.T.Helper()
	rec := e.DoJSON("POST", "/api/v1/orders", fmt.Sprintf(`{
		"customer_id":"cust-0001","payment_term":%q,"supplier_channel":"渠道A",
		"idempotency_key":%q,
		"items":[{"id":%q,"product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`,
		paymentTerm, idemKey, itemID), http.StatusOK, nil)
	var out struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.T.Fatalf("parse create order: %v (%s)", err, rec.Body.String())
	}
	if out.OrderID == "" {
		e.T.Fatalf("empty order id: %s", rec.Body.String())
	}
	return out.OrderID
}

// signalUserOf 从信号 payload 提取 operator 作为 X-User-Id（AUTH-01：E2E 用真实角色投递）。
func signalUserOf(payload string) string {
	var p struct {
		Operator string `json:"operator"`
	}
	if json.Unmarshal([]byte(payload), &p) == nil && p.Operator != "" {
		return p.Operator
	}
	return ""
}

// Signal 投递订单信号并断言 200（按 payload.operator 选择投递角色）。
func (e *Env) Signal(orderID, name, payload string) {
	e.T.Helper()
	headers := map[string]string{}
	if u := signalUserOf(payload); u != "" {
		headers["X-User-Id"] = u
	}
	e.DoJSON("POST", fmt.Sprintf("/api/v1/orders/%s/signal/%s", orderID, name), payload, http.StatusOK, headers)
}

// SignalRetry 投递订单信号：极少数情况下前序测试遗留工作流与 schema 重置交错，
// 订单行短暂不可见（404）——有界重试后仍失败才报错。
func (e *Env) SignalRetry(orderID, name, payload string) {
	e.T.Helper()
	path := fmt.Sprintf("/api/v1/orders/%s/signal/%s", orderID, name)
	headers := map[string]string{}
	if u := signalUserOf(payload); u != "" {
		headers["X-User-Id"] = u
	}
	for i := 0; i < 20; i++ {
		rec := e.Do("POST", path, payload, headers)
		if rec.Code == http.StatusOK {
			return
		}
		if rec.Code == http.StatusNotFound && i < 19 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		e.T.Fatalf("%s %s: status=%d want=%d body=%s", "POST", path, rec.Code, http.StatusOK, rec.Body.String())
	}
}

// SignalAs 以指定用户投递信号。
func (e *Env) SignalAs(userID, orderID, name, payload string) {
	e.T.Helper()
	e.DoJSON("POST", fmt.Sprintf("/api/v1/orders/%s/signal/%s", orderID, name), payload, http.StatusOK, map[string]string{"X-User-Id": userID})
}

// OrderState 查询订单当前状态。
func (e *Env) OrderState(orderID string) string {
	e.T.Helper()
	rec := e.DoJSON("GET", "/api/v1/orders/"+orderID, "", http.StatusOK, nil)
	var out struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.T.Fatalf("parse order state: %v (%s)", err, rec.Body.String())
	}
	return out.State
}

// WaitOrderState 轮询直至订单到达目标状态（默认 15s 超时；本地工作流异步推进）。
func (e *Env) WaitOrderState(orderID, want string) {
	e.WaitOrderStateIn(orderID, want, 15*time.Second)
}

// WaitOrderStateIn 带超时的状态轮询。
func (e *Env) WaitOrderStateIn(orderID, want string, timeout time.Duration) {
	e.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := e.OrderState(orderID); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatalf("order %s: state != %s after %v (last=%s)", orderID, want, timeout, e.OrderState(orderID))
}

// EventCount 返回订单事件链条数（CQ-12 审计）。
func (e *Env) EventCount(orderID string) int {
	e.T.Helper()
	rec := e.DoJSON("GET", "/api/v1/orders/"+orderID+"/events", "", http.StatusOK, nil)
	var out struct {
		Events []repo.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.T.Fatalf("parse events: %v", err)
	}
	return len(out.Events)
}

// demoRoleResolver 演示角色解析（与 cmd/api demoRoleOf 对齐 + CONTACT + 跨租户用户）。
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
	case "u-sales", "dave-op", "u-op", "bob-op":
		return authz.RoleOP, nil
	case "u-contact":
		return authz.RoleContact, nil
	case "u-op-b":
		return authz.RoleOP, nil // tenant-b 商务（跨租户用例）
	}
	return authz.RoleOP, nil
}

// demoRoleOf 与 cmd/api demoRoleOf 对齐（投影用）。
func demoRoleOf(userID string) (authz.Role, string) {
	switch userID {
	case "frank-wh", "u-wh-yangyuan", "u-wh-dongguan":
		return authz.RoleWarehouse, "chang-an"
	case "u-accounting", "erin-acct":
		return authz.RoleAccounting, "chang-an"
	case "u-chairman", "carol-chair":
		return authz.RoleChairman, "chang-an"
	case "u-supervisor", "bob-super":
		return authz.RoleSupervisor, "chang-an"
	case "u-admin", "grace-admin":
		return authz.RoleAdmin, "chang-an"
	case "dave-sales":
		return authz.RoleSales, "chang-an"
	case "u-contact":
		return authz.RoleContact, "chang-an"
	case "u-op-b":
		return authz.RoleOP, "tenant-b" // 跨租户用例
	}
	return authz.RoleOP, "chang-an"
}
