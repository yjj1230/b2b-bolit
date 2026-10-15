package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

// ── M6 门禁：L2 全链路经 HTTP 入口复跑（docs/04-development-plan.html M6 DoD）──
// FakeStore 内存存储 + fakeStarter（记录启动/信号）+ StaticChecker。

func newAPITest(t *testing.T) (*Server, *fakeStarter, *workflows.FakeStore) {
	t.Helper()
	store := workflows.NewFakeStoreForTest()
	starter := newFakeStarter(store)
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return authz.RoleOP, nil })
	roleOf := func(userID string) (authz.Role, string) {
		switch userID {
		case "frank-wh", "u-wh-yangyuan":
			return authz.RoleWarehouse, "chang-an"
		case "u-sales", "carol-sales":
			return authz.RoleSales, "chang-an"
		case "u-supervisor", "bob-super":
			return authz.RoleSupervisor, "chang-an"
		case "u-chairman", "carol-chair":
			return authz.RoleChairman, "chang-an"
		case "u-accounting", "erin-acct":
			return authz.RoleAccounting, "chang-an"
		case "u-admin", "grace-admin":
			return authz.RoleAdmin, "chang-an"
		default:
			return authz.RoleOP, "chang-an"
		}
	}
	srv := NewServer(store, starter, checker, roleOf)
	return srv, starter, store
}

func TestAssistantUIRoutes(t *testing.T) {
	srv, _, _ := newAPITest(t)
	h := srv.Routes()
	for _, path := range []string{"/assistant", "/assistant/?chat_id=chat-cust-0001"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Contains(t, rec.Body.String(), "订单助手 · 销售助理")
	}
}

func doJSON(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// L2 经 HTTP：快速下单 → 全信号 → 查询事件链（L2-01 复跑）
func TestHTTPOrderFullChain(t *testing.T) {
	srv, starter, store := newAPITest(t)
	h := srv.Routes()

	// 预置基础世界
	seedAPIWorld(t, store)

	// 快速下单（幂等启动）
	createBody := `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:http-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`
	rec := doJSON(t, h, "POST", "/api/v1/orders", createBody, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		OrderID string `json:"order_id"`
		State   string `json:"state"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.True(t, created.Created)
	require.Equal(t, domain.StateORDRequested, created.State)

	// 幂等重放：同键 → 返回既有订单，不重复启动
	rec2 := doJSON(t, h, "POST", "/api/v1/orders", createBody, nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var created2 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &created2))
	require.False(t, created2.Created)
	require.Equal(t, created.OrderID, created2.OrderID)
	require.Len(t, starter.started, 1)

	// 审批信号（人工闸门；AUTH-01 信号级角色闸门：按信号使用对应角色）
	signalAs := func(name, payload, userID string) {
		rec := doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/signal/%s", created.OrderID, name), payload,
			map[string]string{"X-User-Id": userID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	signalAs(workflows.SignalPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`, "u-supervisor")
	signalAs(workflows.SignalWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`, "u-wh-yangyuan")
	signalAs(workflows.SignalPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`, "u-accounting")
	signalAs(workflows.SignalShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":100}`, "u-wh-yangyuan")
	signalAs(workflows.SignalInvoiceIssued, `{"verdict":"ISSUED","operator":"u-accounting"}`, "u-accounting")
	signalAs(workflows.SignalPaymentTracked, `{"amount":100000,"operator":"u-accounting"}`, "u-accounting")

	// 未知信号 → 400
	recBad := doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/signal/bogus", created.OrderID), `{}`, nil)
	require.Equal(t, http.StatusBadRequest, recBad.Code)

	// 事件链查询（CQ-12）
	recEv := doJSON(t, h, "GET", fmt.Sprintf("/api/v1/orders/%s/events", created.OrderID), "", nil)
	require.Equal(t, http.StatusOK, recEv.Code)
	var events struct {
		Events []repo.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(recEv.Body.Bytes(), &events))
	require.NotEmpty(t, events.Events)

	// 待办检索：全部已裁决 → 无 PENDING
	recTodos := doJSON(t, h, "GET", "/api/v1/todos?state=PENDING", "", nil)
	require.Equal(t, http.StatusOK, recTodos.Code)
	var todos struct {
		Tasks []repo.ApprovalTask `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(recTodos.Body.Bytes(), &todos))
	require.Empty(t, todos.Tasks)
}

// 订单查询投影：仓管视图剥离价格（L1-10 经 HTTP）
func TestHTTPWarehouseProjection(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)
	ord := createAPIOrder(t, h)

	// 仓管视图：无价格字段
	recWh := doJSON(t, h, "GET", "/api/v1/orders/"+ord, "", map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusOK, recWh.Code)
	var wh struct {
		UnitPriceCents *int64 `json:"unit_price_cents"`
		AmountCents    *int64 `json:"amount_cents"`
	}
	require.NoError(t, json.Unmarshal(recWh.Body.Bytes(), &wh))
	require.Nil(t, wh.UnitPriceCents, "warehouse must not see price")
	require.Nil(t, wh.AmountCents)

	// 商务视图：保留价格
	recOp := doJSON(t, h, "GET", "/api/v1/orders/"+ord, "", map[string]string{"X-User-Id": "dave-op"})
	require.Equal(t, http.StatusOK, recOp.Code)
	var op struct {
		UnitPriceCents *int64 `json:"unit_price_cents"`
		AmountCents    *int64 `json:"amount_cents"`
	}
	require.NoError(t, json.Unmarshal(recOp.Body.Bytes(), &op))
	require.NotNil(t, op.UnitPriceCents)
	require.NotNil(t, op.AmountCents)

	// 404
	rec404 := doJSON(t, h, "GET", "/api/v1/orders/ORD-nope", "", nil)
	require.Equal(t, http.StatusNotFound, rec404.Code)
}

// 预订单经 HTTP：消息 → 预订单 → 确认信号 → 派生正式订单（L2-12 复跑）
func TestHTTPPreOrderChain(t *testing.T) {
	srv, starter, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)

	rec := doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"msg-1","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
		State      string `json:"state"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))
	require.True(t, pre.Created)
	require.Equal(t, domain.StatePreCaptured, pre.State)

	// 消息幂等：同 msgid 重复 → 返回既有
	rec2 := doJSON(t, h, "POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"msg-1","intent_version":"v1","candidates":[]}`, nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var pre2 struct {
		PreOrderID string `json:"pre_order_id"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &pre2))
	require.False(t, pre2.Created)
	require.Equal(t, pre.PreOrderID, pre2.PreOrderID)

	// 确认信号（N-02：销售是唯一可确认角色）
	recConf := doJSON(t, h, "POST", fmt.Sprintf("/api/v1/preorders/%s/signal/%s", pre.PreOrderID, workflows.SignalPreOrderConfirm),
		`{"confirmed":true,"operator":"u-sales"}`, map[string]string{"X-User-Id": "u-sales"})
	require.Equal(t, http.StatusOK, recConf.Code, recConf.Body.String())
	require.Len(t, starter.signals, 1)
	// 非销售角色确认 → 403（N-02）
	rec403 := doJSON(t, h, "POST", fmt.Sprintf("/api/v1/preorders/%s/signal/%s", pre.PreOrderID, workflows.SignalPreOrderConfirm),
		`{"confirmed":true,"operator":"u-wh"}`, map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusForbidden, rec403.Code, rec403.Body.String())

	// 预订单查询
	recGet := doJSON(t, h, "GET", "/api/v1/preorders/"+pre.PreOrderID, "", nil)
	require.Equal(t, http.StatusOK, recGet.Code)
}

// healthz
func TestHTTPHealth(t *testing.T) {
	srv, _, _ := newAPITest(t)
	rec := doJSON(t, srv.Routes(), "GET", "/healthz", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// ── 测试辅助 ──

func seedAPIWorld(t *testing.T, store *workflows.FakeStore) {
	t.Helper()
	store.SeedTenant("chang-an")
	store.SeedCustomer("chang-an", "cust-0001", "A")
	store.SeedInventory("chang-an", "wh-yangyuan", "prod-c68", 1000)
}

func createAPIOrder(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:proj-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	return created.OrderID
}
