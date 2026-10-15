package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/bridge"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
)

// TestBridgeHandlerEndpoints 用真实旧系统契约的假服务跑通三个桥接端点。
func TestBridgeHandlerEndpoints(t *testing.T) {
	captured := url.Values{}
	var capturedFileParts int
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/approval/feishu-user-by-code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"name": "陈虹励", "open_id": "ou_hongli", "user_id": "u-hongli", "union_id": "on_1",
				},
				"role":  "agent",
				"token": "bridge-token",
			})
		case "/external-orders/create":
			if r.Header.Get("Authorization") != "Bearer bridge-token" {
				t.Errorf("missing bearer")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
			} else {
				for key, values := range r.MultipartForm.Value {
					captured[key] = append([]string(nil), values...)
				}
				for _, files := range r.MultipartForm.File {
					capturedFileParts += len(files)
				}
			}
			w.Header().Set("Location", "/external-orders/inst-1?created=1&submitter_token=secret")
			w.WriteHeader(http.StatusSeeOther)
		case "/external-orders/submission-status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "done", "instance_id": "inst-1",
				"flow_id": "SO-20260821-0001", "detail_url": "/flows/SO-20260821-0001",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer old.Close()

	bc, err := bridge.New(bridge.Config{BaseURL: old.URL, AppID: "cli_test", RedirectURI: "https://v01.example/cb"})
	if err != nil {
		t.Fatalf("bridge new: %v", err)
	}
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Bridge = bc
	h := srv.Routes()

	// auth-url
	rec := doJSONRequest(h, "GET", "/api/v1/bridge/auth-url?state=s1", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "open.feishu.cn") {
		t.Fatalf("auth-url: %d %s", rec.Code, rec.Body.String())
	}
	// callback
	rec = doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"auth-code-1"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "u-hongli") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	// create order
	body := `{
		"customer_record_id":"rec_cust","customer_name":"北京华信科技","customer_level":"A",
		"supplier":"渠道A","payment_terms":"款到发货","remittance_method":"转账","currency":"EUR",
		"shipping_address":"上海仓库","receiver":"张三","phone":"13800000000",
		"sales_name":"陈虹励","sales_open_id":"ou_hongli","sales_user_id":"u-hongli","sales_role_key":"agent",
		"items":[{"product_name":"标准电机 C68","quantity":100,"unit_price":12.3456,"unit_price_cents":1235,
		  "packaging":"长安仁恒包装","packing_remark":"小袋套吨袋","package_spec":"25",
		  "quality_report_required":true,"arrival_date":"2026-09-01"}]
	}`
	rec = doJSONRequest(h, "POST", "/api/v1/bridge/orders", body, nil)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "SO-20260821-0001") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if captured.Get("currency") != "EUR" || captured.Get("customer_contract_no") != "" || capturedFileParts != 0 {
		t.Fatalf("multipart contract/currency mismatch: currency=%q contract=%q files=%d", captured.Get("currency"), captured.Get("customer_contract_no"), capturedFileParts)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(captured.Get("items_json")), &items); err != nil || len(items) != 1 {
		t.Fatalf("items_json: %q err=%v", captured.Get("items_json"), err)
	}
	got := items[0]
	if got["unit_price"] != 12.3456 || got["packaging"] != "长安仁恒包装" || got["package_spec"] != "25" || got["arrival_date"] != "2026-09-01" || got["remark"] != "小袋套吨袋；需要质检报告" {
		t.Fatalf("mapped item = %#v", got)
	}
}

func TestBridgeHandlerRejectsUnknownCurrency(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/approval/feishu-user-by-code" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"user_id": "u-hongli"}, "token": "bridge-token"})
			return
		}
		t.Fatal("invalid currency must not reach old system")
	}))
	defer old.Close()
	bc, _ := bridge.New(bridge.Config{BaseURL: old.URL, AppID: "cli_test", RedirectURI: "https://v01.example/cb"})
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Bridge = bc
	h := srv.Routes()
	_ = doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"auth-code-1"}`, nil)
	rec := doJSONRequest(h, "POST", "/api/v1/bridge/orders", `{"currency":"GBP"}`, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "currency") {
		t.Fatalf("unknown currency: %d %s", rec.Code, rec.Body.String())
	}
}

// TestBridgeHandlersDisabled 未启用桥接时返回 503。
func TestBridgeHandlersDisabled(t *testing.T) {
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	h := srv.Routes()
	if rec := doJSONRequest(h, "GET", "/api/v1/bridge/auth-url", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("auth-url disabled = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"x"}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("callback disabled = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/orders", `{}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("orders disabled = %d", rec.Code)
	}
}

func TestBridgeSalesPersonsUsesLocalProviderWhenBridgeDisabled(t *testing.T) {
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.LocalSalesPersons = fakeLocalSalesPersons{items: []bridge.SalesPerson{
		{Name: "销售-张三", UserID: "u-sales", OpenID: "wecom_sales", RoleKey: "sales"},
	}}
	rec := doJSONRequest(srv.Routes(), "GET", "/api/v1/bridge/sales-persons", "", map[string]string{"X-User-Id": "u-op"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "销售-张三") {
		t.Fatalf("local sales persons: %d %s", rec.Code, rec.Body.String())
	}
}

type fakeLocalSalesPersons struct {
	items []bridge.SalesPerson
}

func (f fakeLocalSalesPersons) ListSalesPersons(context.Context, string) ([]bridge.SalesPerson, error) {
	return f.items, nil
}

func doJSONRequest(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type fakeBridgeLedger struct {
	items []repo.BridgeSubmission
}

func (f *fakeBridgeLedger) ListBridgeSubmissions(ctx context.Context, tenantID string, limit int) ([]repo.BridgeSubmission, error) {
	return f.items, nil
}

// BRG-01 收敛：桥接「我的订单」列表端点与两个兜底页面处理器补测试证据。
func TestBridgeListOrdersAndUIs(t *testing.T) {
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.BridgeLedger = &fakeBridgeLedger{items: []repo.BridgeSubmission{
		{OrderID: "BRG-1", FlowID: "SO-20260821-0001", InstanceID: "inst-1", DetailURL: "/flows/SO-20260821-0001"},
	}}
	h := srv.Routes()

	rec := doJSONRequest(h, "GET", "/api/v1/bridge/orders", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SO-20260821-0001") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/bridge", "/bridge/", "/assistant", "/assistant/?group_id=g1"} {
		rec := doJSONRequest(h, "GET", path, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if len(rec.Body.String()) < 100 {
			t.Fatalf("%s: body too short", path)
		}
	}
}

// TestBridgeCallbackBranches 覆盖回调端点的错误分支：坏 JSON / 缺 code / 授权过期 / 上游失败。
func TestBridgeCallbackBranches(t *testing.T) {
	newSrv := func(handler http.HandlerFunc) http.Handler {
		old := httptest.NewServer(handler)
		t.Cleanup(old.Close)
		bc, err := bridge.New(bridge.Config{BaseURL: old.URL, AppID: "cli", RedirectURI: "https://v01.example/cb"})
		if err != nil {
			t.Fatalf("bridge new: %v", err)
		}
		srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
		srv.Bridge = bc
		return srv.Routes()
	}

	h := newSrv(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"auth expired"}`))
	})
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty code = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth expired = %d body=%s", rec.Code, rec.Body.String())
	}

	h2 := newSrv(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	if rec := doJSONRequest(h2, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream = %d", rec.Code)
	}

	h3 := newSrv(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	})
	if rec := doJSONRequest(h3, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("malformed upstream = %d", rec.Code)
	}
}

// TestBridgeCreateOrderBranches 覆盖下单端点的鉴权/坏 JSON/授权过期/上游失败分支。
func TestBridgeCreateOrderBranches(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/approval/feishu-user-by-code" {
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"虹励","user_id":"u-h","open_id":"ou-h"},"role":"agent","token":"t"}`))
			return
		}
		if r.URL.Path == "/external-orders/create" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"FEISHU_AUTH_EXPIRED"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	bc, _ := bridge.New(bridge.Config{BaseURL: old.URL, AppID: "cli", RedirectURI: "https://v01.example/cb"})
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Bridge = bc
	h := srv.Routes()

	valid := `{"customer_name":"北京华信","items":[{"product_name":"P","quantity":1,"unit_price_cents":100}]}`
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/orders", `{`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/orders", valid, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/orders", valid, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("upstream auth expired = %d body=%s", rec.Code, rec.Body.String())
	}

	old2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/approval/feishu-user-by-code" {
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"虹励","user_id":"u-h","open_id":"ou-h"},"role":"agent","token":"t"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer old2.Close()
	bc2, _ := bridge.New(bridge.Config{BaseURL: old2.URL, AppID: "cli", RedirectURI: "https://v01.example/cb"})
	srv2 := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv2.Bridge = bc2
	h2 := srv2.Routes()
	if rec := doJSONRequest(h2, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("callback2 = %d", rec.Code)
	}
	if rec := doJSONRequest(h2, "POST", "/api/v1/bridge/orders", valid, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 5xx = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestBridgeSalesPersonsEndpoint 覆盖可选归属销售端点（BRG-01 分支）。
func TestBridgeSalesPersonsEndpoint(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/approval/feishu-user-by-code":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"虹励","user_id":"u-h","open_id":"ou-h"},"role":"agent","token":"t"}`))
		case "/api/approval/sales-persons":
			_, _ = w.Write([]byte(`{"ok":true,"items":[{"name":"张有连","user_id":"u-sales","open_id":"ou-sales","role_key":"sales"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer old.Close()
	bc, _ := bridge.New(bridge.Config{BaseURL: old.URL, AppID: "cli", RedirectURI: "https://v01.example/cb"})
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Bridge = bc
	h := srv.Routes()

	if rec := doJSONRequest(h, "GET", "/api/v1/bridge/sales-persons", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d", rec.Code)
	}
	if rec := doJSONRequest(h, "POST", "/api/v1/bridge/callback", `{"code":"c"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d", rec.Code)
	}
	rec := doJSONRequest(h, "GET", "/api/v1/bridge/sales-persons", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "张有连") {
		t.Fatalf("sales persons = %d %s", rec.Code, rec.Body.String())
	}
}

// TestBridgeListOrdersNormalized 覆盖 nil 台账归一化为空数组。
func TestBridgeListOrdersNormalized(t *testing.T) {
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.BridgeLedger = &fakeBridgeLedger{items: nil}
	rec := doJSONRequest(srv.Routes(), "GET", "/api/v1/bridge/orders", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("nil list = %d %s", rec.Code, rec.Body.String())
	}
}

// 方案B：新客户下单后客户档案落备库 pgAPI（幂等查重；必填缺失/已存在跳过）。
func TestEnsurePgapiCustomerArchive(t *testing.T) {
	m := orderdata.NewMemoryProvider()
	if _, err := m.CreateCustomer(context.Background(), "chang-an", pgapi.CustomerWrite{
		CustomerName: "已存在客户", CustomerLevel: "A", Supplier: "渠道A",
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.Data = m

	// 新客户 → 建档成功，按名称可搜到，且按销售员姓名可搜到
	srv.ensurePgapiCustomerArchive(context.Background(), "chang-an", bridgeOrderRequest{
		CustomerName: "上海海逸科贸有限公司", CustomerLevel: "B", Supplier: "上海分公司",
		TaxNo: "91310000", Phone: "13800000000", ShippingAddress: "上海仓库", Receiver: "张三",
		SalesName: "王杰", SalesOpenID: "ou_wangjie", PaymentTerms: "款到",
	})
	res, err := m.SearchCustomers(context.Background(), "chang-an", pgapi.Query{Q: "上海海逸"})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("新客户应建档: items=%d err=%v", len(res.Items), err)
	}
	if res.Items[0].PaymentTerms != "款到" {
		t.Fatalf("归档应带付款方式: got %q", res.Items[0].PaymentTerms)
	}
	// 归档客户带销售员：按销售员姓名/未命中 open_id 均应能搜到
	res, _ = m.SearchCustomers(context.Background(), "chang-an", pgapi.Query{
		Filters: map[string]string{"salesperson": "王杰"},
	})
	if len(res.Items) != 1 {
		t.Fatalf("归档客户应按销售员可搜到: %d", len(res.Items))
	}

	// 已存在同名 → 跳过（不重复建档）
	srv.ensurePgapiCustomerArchive(context.Background(), "chang-an", bridgeOrderRequest{
		CustomerName: "上海海逸科贸有限公司", CustomerLevel: "A", Supplier: "上海分公司",
	})
	res, _ = m.SearchCustomers(context.Background(), "chang-an", pgapi.Query{Q: "上海海逸"})
	if len(res.Items) != 1 {
		t.Fatalf("同名不应重复建档: %d", len(res.Items))
	}

	// 必填缺失（无 supplier）→ 跳过
	srv.ensurePgapiCustomerArchive(context.Background(), "chang-an", bridgeOrderRequest{CustomerName: "无供方客户"})
	res, _ = m.SearchCustomers(context.Background(), "chang-an", pgapi.Query{Q: "无供方"})
	if len(res.Items) != 0 {
		t.Fatalf("缺 supplier 不应建档: %d", len(res.Items))
	}
}
