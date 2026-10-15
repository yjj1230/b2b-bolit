package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c, err := New(Config{BaseURL: ts.URL, AppID: "cli_test", RedirectURI: "https://v01.example/cb"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ts
}

func TestAuthorizationURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	got := c.AuthorizationURL("state-1")
	want := "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_test&redirect_uri=https%3A%2F%2Fv01.example%2Fcb&state=state-1"
	if got != want {
		t.Fatalf("url = %s", got)
	}
}

func TestListSalesPersons(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/approval/sales-persons" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer create-token-1" {
			t.Fatalf("auth header = %s", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"items": []map[string]any{
				{"name": "王杰", "user_id": "g49266b7", "open_id": "ou_wangjie", "role_key": "SALES"},
				{"name": "陈虹励", "user_id": "673gadg4", "open_id": "ou_hongli", "role_key": "SALES"},
			},
		})
	})
	got, err := c.ListSalesPersons(context.Background(), "create-token-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Name != "王杰" || got[0].UserID != "g49266b7" || got[1].OpenID != "ou_hongli" {
		t.Fatalf("items = %+v", got)
	}
	if _, err := c.ListSalesPersons(context.Background(), ""); err == nil {
		t.Fatalf("expected error for empty token")
	}
}

func TestExchangeCodeAndCache(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/approval/feishu-user-by-code" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var in struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Code != "code-1" {
			t.Errorf("code = %s", in.Code)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"name": "陈虹励", "open_id": "ou_hongli", "user_id": "u-hongli", "union_id": "on_1",
			},
			"role":  "agent",
			"token": "create-token-1",
		})
	})
	got, err := c.ExchangeCode(context.Background(), "u-hongli", "code-1")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got.Token != "create-token-1" || got.Identity.Role != "agent" || got.Identity.Name != "陈虹励" {
		t.Fatalf("exchange result: %+v", got)
	}
	tok, exp, ok := c.TokenFor("u-hongli")
	if !ok || tok != "create-token-1" || exp.Sub(time.Now()) < TokenTTL-ReauthWindow {
		t.Fatalf("cache: tok=%s exp=%s ok=%v", tok, exp, ok)
	}
}

func TestCreateOrderMultipartAndPoll(t *testing.T) {
	var gotAuth string
	gotForm := url.Values{}
	var gotFileParts int
	var pollCount int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/external-orders/create":
			gotAuth = r.Header.Get("Authorization")
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				t.Errorf("content type = %s", r.Header.Get("Content-Type"))
			}
			_ = r.ParseMultipartForm(1 << 20)
			for k, values := range r.MultipartForm.Value {
				gotForm[k] = append([]string(nil), values...)
			}
			for _, files := range r.MultipartForm.File {
				gotFileParts += len(files)
			}
			w.Header().Set("Location", "/external-orders/inst-1?created=1&submitter_token=secret")
			w.WriteHeader(http.StatusSeeOther)
		case "/external-orders/submission-status":
			pollCount++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "done", "order_id": "ORD-1", "instance_id": "inst-1",
				"flow_id": "SO-20260821-0001", "detail_url": "/flows/SO-20260821-0001",
			})
		}
	})
	sub, err := c.CreateOrder(context.Background(), "create-token-1", OrderInput{
		TenantID: "chang-an", OrderID: "ORD-1", CustomerRecordID: "rec_cust",
		CustomerName: "北京华信科技", CustomerLevel: "A", Supplier: "渠道A",
		Items:        []Item{{ProductName: "标准电机 C68", Quantity: 100, UnitPrice: 12.5, Packaging: "长安仁恒包装", PackageSpec: "25", ArrivalDate: "2026-09-01", Remark: "小袋套吨袋；需要质检报告"}},
		PaymentTerms: "款到发货", RemittanceMethod: "转账", Currency: "USD",
		ShippingAddress: "上海仓库", Receiver: "张三", Phone: "13800000000",
		Sales: Identity{Name: "虹励", OpenID: "ou_h", UserID: "u_h", Role: "agent"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sub.FlowID != "SO-20260821-0001" || sub.InstanceID != "inst-1" || gotAuth != "Bearer create-token-1" {
		t.Fatalf("sub=%+v auth=%s", sub, gotAuth)
	}
	if gotForm.Get("create_idempotency_key") != "B2B_V01_chang-an:ORD-1" ||
		gotForm.Get("customer_mode") != "EXISTING" || gotForm.Get("customer_existing_record_id") != "rec_cust" ||
		gotForm.Get("payment_terms") != "款到发货" || gotForm.Get("sales_role_key") != "agent" || gotForm.Get("currency") != "USD" {
		t.Fatalf("form = %#v", gotForm)
	}
	if gotForm.Get("customer_contract_no") != "" || gotFileParts != 0 {
		t.Fatalf("contract capability leaked: field=%q files=%d", gotForm.Get("customer_contract_no"), gotFileParts)
	}
	var items []Item
	if err := json.Unmarshal([]byte(gotForm.Get("items_json")), &items); err != nil || len(items) != 1 {
		t.Fatalf("items_json=%q err=%v", gotForm.Get("items_json"), err)
	}
	if items[0].Packaging != "长安仁恒包装" || items[0].PackageSpec != "25" || items[0].ArrivalDate != "2026-09-01" || items[0].Remark != "小袋套吨袋；需要质检报告" {
		t.Fatalf("item = %+v", items[0])
	}
	if pollCount < 1 {
		t.Fatal("submission-status not polled")
	}
}

func TestCreateOrderErrors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"FEISHU_AUTH_EXPIRED"}`))
	})
	if _, err := c.CreateOrder(context.Background(), "tok", OrderInput{TenantID: "t", OrderID: "o"}); !IsAuthExpired(err) {
		t.Fatalf("expected auth expired, got %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"idempotency conflict"}`))
	}))
	defer ts.Close()
	c2, _ := New(Config{BaseURL: ts.URL, AppID: "a", RedirectURI: "r"})
	_, err := c2.CreateOrder(context.Background(), "tok", OrderInput{TenantID: "t", OrderID: "o"})
	if err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("conflict err = %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing config must fail")
	}
	if _, err := New(Config{BaseURL: "http://x", AppID: "a"}); err == nil {
		t.Fatal("missing redirect must fail")
	}
}

func TestMemoryTokenCacheExpiry(t *testing.T) {
	m := NewMemoryTokenCache()
	now := time.Now()
	_ = m.Put("u", "t1", now.Add(-time.Minute))
	if _, _, ok := m.Get("u", now); ok {
		t.Fatal("expired token must not return")
	}
	_ = m.Put("u", "t2", now.Add(TokenTTL))
	tok, _, ok := m.Get("u", now)
	if !ok || tok != "t2" {
		t.Fatalf("tok=%s ok=%v", tok, ok)
	}
}

func TestIdempotencyKeyPrefix(t *testing.T) {
	in := OrderInput{TenantID: "chang-an", OrderID: "ORD-9"}
	key := in.IdempotencyKey()
	if !strings.HasPrefix(key, "B2B_V01_") {
		t.Fatalf("key = %s", key)
	}
	if _, err := url.ParseQuery("k=" + key); err != nil {
		t.Fatalf("not url-safe: %v", err)
	}
}

// BRG-01 收敛：ExchangeCode 错误分支（401/403/502/坏 JSON/网络错误）补测试证据。
func TestExchangeCodeErrorBranches(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrorKind
		plain  string // 非 *Error 时断言错误文本包含该串
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"auth expired"}`, ErrAuthExpired, ""},
		{"forbidden", http.StatusForbidden, `{"message":"no permission"}`, ErrAuthExpired, ""},
		// 旧系统 502 按契约视为授权/通道失效（exchange 当前语义）
		{"bad gateway", http.StatusBadGateway, `{"message":"down"}`, ErrAuthExpired, ""},
		{"bad json ok", http.StatusOK, `{bad`, "", "decode exchange response"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer ts.Close()
			bc, _ := New(Config{BaseURL: ts.URL, AppID: "a", RedirectURI: "r"})
			_, err := bc.ExchangeCode(context.Background(), "u", "code")
			if err == nil {
				t.Fatal("exchange must fail")
			}
			if c.plain != "" {
				if !strings.Contains(err.Error(), c.plain) {
					t.Fatalf("err = %v, want contains %q", err, c.plain)
				}
				return
			}
			if be, ok := err.(*Error); !ok || be.Kind != c.want {
				t.Fatalf("err = %v, want kind %s", err, c.want)
			}
		})
	}
	// 网络错误（连接拒绝）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	bc, _ := New(Config{BaseURL: url, AppID: "a", RedirectURI: "r"})
	if _, err := bc.ExchangeCode(context.Background(), "u", "code"); err == nil {
		t.Fatal("network error must fail")
	}
}

// BRG-01 收敛：旧系统错误分类（classifyError）全部分支有测试证据。
func TestClassifyError(t *testing.T) {
	cases := []struct {
		msg  string
		kind ErrorKind
	}{
		{"token expired FEISHU_AUTH_EXPIRED", ErrAuthExpired},
		{"oauth auth failed", ErrAuthExpired},
		{"授权已过期", ErrAuthExpired},
		{"ORDER_DATA_INVALID: quantity missing", ErrOrderDataInvalid},
		{"字段校验失败", ErrOrderDataInvalid},
		{"SERVICE_MISCONFIGURED: app missing", ErrServiceMisconfigured},
		{"配置错误", ErrServiceMisconfigured},
		{"idempotency duplicate", ErrIdempotencyConflict},
		{"幂等冲突", ErrIdempotencyConflict},
		{"something else", ErrUnknown},
		{"", ErrUnknown},
	}
	for _, c := range cases {
		got := classifyError(400, c.msg)
		if got.Kind != c.kind {
			t.Fatalf("classify(%q) = %s, want %s", c.msg, got.Kind, c.kind)
		}
		if IsAuthExpired(got) != (c.kind == ErrAuthExpired) {
			t.Fatalf("IsAuthExpired(%q) = %v", c.msg, IsAuthExpired(got))
		}
	}
	if e := classifyError(400, "x"); e.Error() != "bridge: BRIDGE_ERROR: x" {
		t.Fatalf("error text = %s", e.Error())
	}
	if e := (&Error{Kind: ErrUnknown}).Error(); e != "bridge: BRIDGE_ERROR" {
		t.Fatalf("empty message error text = %s", e)
	}
	// 未知错误不是授权过期，普通 error 也不是
	if IsAuthExpired(nil) || IsAuthExpired(&Error{Kind: ErrOrderDataInvalid}) {
		t.Fatal("wrong auth-expired classification")
	}
}
