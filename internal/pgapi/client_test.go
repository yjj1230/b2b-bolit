package pgapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSearchProducts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderAPIKey) != "test-key" {
			t.Errorf("missing api key header")
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/db/products/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "BT" || r.URL.Query().Get("page_size") != "10" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items": []map[string]any{{"record_id": "rec1", "product_name": "BTD-28", "spec": "25kg"}},
				"page":  1, "page_size": 10, "total": 1,
				"pagination": map[string]any{"page": 1, "page_size": 10, "total": 1, "has_next": false},
			},
			"message": "ok",
		})
	}))
	defer ts.Close()

	c, err := NewClient(ts.URL, "test-key", nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.SearchProducts(context.Background(), Query{Q: "BT", PageSize: 10})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].RecordID != "rec1" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestClientCreateAndUpdateCustomer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"record_id": "rec_cust", "customer_name": "测试客户", "customer_level": "A"},
				"message": "ok",
			})
			return
		}
		if r.Method == http.MethodPatch {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"record_id": "rec_cust", "customer_name": "测试客户", "customer_level": "B"},
				"message": "ok",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c, _ := NewClient(ts.URL, "key", nil)
	created, err := c.CreateCustomer(context.Background(), CustomerWrite{
		CustomerName: "测试客户", CustomerLevel: "A", CustomerType: "企业客户", Supplier: "供方",
	})
	if err != nil || created.RecordID != "rec_cust" {
		t.Fatalf("CreateCustomer: %+v err=%v", created, err)
	}
	level := "B"
	updated, err := c.UpdateCustomer(context.Background(), "rec_cust", CustomerPatch{CustomerLevel: &level})
	if err != nil || updated.CustomerLevel != "B" {
		t.Fatalf("UpdateCustomer: %+v err=%v", updated, err)
	}
}

func TestClientHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid or missing API key"}`))
	}))
	defer ts.Close()

	c, _ := NewClient(ts.URL, "bad", nil)
	_, err := c.GetCustomer(context.Background(), "rec1")
	if err == nil {
		t.Fatalf("expected unauthorized error")
	}
	if IsNotFound(err) {
		t.Fatalf("401 should not be classified as not found: %v", err)
	}
}

func TestQueryDefaultsAndCap(t *testing.T) {
	v := buildQuery(Query{PageSize: 999, Filters: map[string]string{"customer_name": "科技"}})
	if v.Get("page") != "1" || v.Get("page_size") != "200" || v.Get("customer_name") != "科技" {
		t.Fatalf("query = %s", v.Encode())
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("", "k", nil); err == nil {
		t.Fatal("expected empty base URL error")
	}
	if _, err := NewClient("http://x", "", nil); err == nil {
		t.Fatal("expected empty api key error")
	}
	c, err := NewClient("http://example.com/", "k", nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.BaseURL() != "http://example.com" {
		t.Fatalf("BaseURL = %q", c.BaseURL())
	}
}

func TestClientSingleRecordEndpoints(t *testing.T) {
	var gotPath, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		switch r.URL.Path {
		case "/api/db/products/rec-p":
			writeEnvelope(w, map[string]any{"record_id": "rec-p", "product_name": "P"})
		case "/api/db/customers/rec-c":
			writeEnvelope(w, map[string]any{"record_id": "rec-c", "customer_name": "C", "customer_level": "A"})
		case "/api/db/customers/rec-c/addresses":
			writeEnvelope(w, map[string]any{"record_id": "rec-c", "customer_name": "C", "phone": "138"})
		case "/api/db/purchase-contracts/rec-ct":
			writeEnvelope(w, map[string]any{"record_id": "rec-ct", "contract_no": "HT-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	ctx := context.Background()

	p, err := c.GetProduct(ctx, "rec-p")
	if err != nil || p.ProductName != "P" {
		t.Fatalf("GetProduct: %+v err=%v", p, err)
	}
	if gotPath != "/api/db/products/rec-p" || gotMethod != http.MethodGet {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	cust, err := c.GetCustomer(ctx, "rec-c")
	if err != nil || cust.CustomerLevel != "A" {
		t.Fatalf("GetCustomer: %+v err=%v", cust, err)
	}
	addr, err := c.GetCustomerAddresses(ctx, "rec-c")
	if err != nil || addr.Phone != "138" {
		t.Fatalf("GetCustomerAddresses: %+v err=%v", addr, err)
	}
	ct, err := c.GetPurchaseContract(ctx, "rec-ct")
	if err != nil || ct.ContractNo != "HT-1" {
		t.Fatalf("GetPurchaseContract: %+v err=%v", ct, err)
	}
}

func TestClientListEndpoints(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeListEnvelope(w, r.URL.Path)
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	ctx := context.Background()

	custs, err := c.SearchCustomers(ctx, Query{PageSize: 5})
	if err != nil || custs.Total != 1 || len(custs.Items) != 1 || custs.Items[0].CustomerName != "C" {
		t.Fatalf("SearchCustomers: %+v err=%v", custs, err)
	}
	if gotPath != "/api/db/customers/search" {
		t.Fatalf("path = %s", gotPath)
	}
	cts, err := c.SearchPurchaseContracts(ctx, Query{PageSize: 5})
	if err != nil || cts.Total != 1 || cts.Items[0].ContractNo != "HT-1" {
		t.Fatalf("SearchPurchaseContracts: %+v err=%v", cts, err)
	}
}

func TestClientProductWrites(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		writeEnvelope(w, map[string]any{"record_id": "rec-p", "product_name": "P", "spec": "25kg"})
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	ctx := context.Background()

	p, err := c.CreateProduct(ctx, ProductWrite{ProductName: "P", Spec: "25kg"})
	if err != nil || p.RecordID != "rec-p" {
		t.Fatalf("CreateProduct: %+v err=%v", p, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/db/products" || gotBody == "" {
		t.Fatalf("create: %s %s body=%s", gotMethod, gotPath, gotBody)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil || payload["spec"] != "25kg" {
		t.Fatalf("create body: %s err=%v", gotBody, err)
	}

	spec := "20kg"
	p, err = c.UpdateProduct(ctx, "rec-p", ProductPatch{Spec: &spec})
	if err != nil || p.Spec != "25kg" {
		t.Fatalf("UpdateProduct: %+v err=%v", p, err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/db/products/rec-p" {
		t.Fatalf("update: %s %s", gotMethod, gotPath)
	}
}

func TestClientDecodeErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/db/products/search":
			_, _ = w.Write([]byte(`{"success":true,"data":`))
		case "/api/db/products/rec-p":
			_, _ = w.Write([]byte(`{"success":true,"data":`))
		case "/api/db/products":
			_, _ = w.Write([]byte(`{"success":true,"data":`))
		case "/api/db/customers/search":
			writeListEnvelope(w, r.URL.Path)
		default:
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
		}
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	ctx := context.Background()

	if _, err := c.SearchProducts(ctx, Query{}); err == nil {
		t.Fatal("expected list decode error")
	}
	if _, err := c.GetProduct(ctx, "rec-p"); err == nil {
		t.Fatal("expected get decode error")
	}
	if _, err := c.CreateProduct(ctx, ProductWrite{ProductName: "P", Spec: "S"}); err == nil {
		t.Fatal("expected write decode error")
	}
}

func TestClientBusinessFailureEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error_code":"BUSINESS_ERR","message":"bad data","request_id":"req-1"}`))
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	_, err := c.SearchCustomers(context.Background(), Query{})
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("expected *Error, got %T %v", err, err)
	}
	if perr.StatusCode != http.StatusOK || perr.Code != "BUSINESS_ERR" || perr.Message != "bad data" || perr.RequestID != "req-1" {
		t.Fatalf("unexpected error: %+v", perr)
	}

	// success=false 且 message 为空时回退到 detail 字段
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"detail":"fallback detail"}`))
	}))
	defer ts2.Close()
	c2, _ := NewClient(ts2.URL, "k", nil)
	_, err = c2.GetCustomer(context.Background(), "rec-c")
	perr, ok := err.(*Error)
	if !ok || perr.Message != "fallback detail" {
		t.Fatalf("detail fallback: %+v", err)
	}
}

func TestClientRequestErrors(t *testing.T) {
	// 网络失败（服务已关闭）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	c, _ := NewClient(url, "k", nil)
	if _, err := c.GetProduct(context.Background(), "rec-p"); err == nil {
		t.Fatal("expected network error")
	}

	// 非法 base URL（NewClient 只校验非空，实际请求在 http.NewRequestWithContext 失败）
	bad, err := NewClient("://bad", "k", nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := bad.GetCustomer(context.Background(), "rec-c"); err == nil {
		t.Fatal("expected new request error")
	}

	// context 已取消
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts2.Close()
	c2, _ := NewClient(ts2.URL, "k", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c2.GetCustomer(ctx, "rec-c"); err == nil {
		t.Fatal("expected context canceled error")
	}
}

func TestClientHTTPErrorEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"error_code":"E1","message":"m1","request_id":"r1"}`))
	}))
	defer ts.Close()
	c, _ := NewClient(ts.URL, "k", nil)
	_, err := c.GetProduct(context.Background(), "rec-p")
	perr, ok := err.(*Error)
	if !ok || perr.StatusCode != http.StatusBadRequest || perr.Code != "E1" || perr.Message != "m1" || perr.RequestID != "r1" {
		t.Fatalf("http error envelope: %+v", err)
	}
}

func TestBuildQueryBranches(t *testing.T) {
	v := buildQuery(Query{})
	if v.Get("page") != "1" || v.Get("page_size") != "20" || len(v) != 2 {
		t.Fatalf("defaults: %s", v.Encode())
	}
	v = buildQuery(Query{Page: -3, PageSize: -1})
	if v.Get("page") != "1" || v.Get("page_size") != "20" {
		t.Fatalf("negative defaults: %s", v.Encode())
	}
	v = buildQuery(Query{SortBy: "created_time", SortOrder: "asc", IncludeDeleted: true, Filters: map[string]string{"empty": ""}})
	if v.Get("sort_by") != "created_time" || v.Get("sort_order") != "asc" || v.Get("include_deleted") != "true" {
		t.Fatalf("sort/include: %s", v.Encode())
	}
	if v.Has("empty") {
		t.Fatalf("empty filter should be skipped: %s", v.Encode())
	}
}

func TestErrorHelpers(t *testing.T) {
	e := &Error{StatusCode: 404, Message: "not found"}
	if got := e.Error(); got != "pgapi 404: not found" {
		t.Fatalf("Error() = %q", got)
	}
	e = &Error{StatusCode: 400, Code: "E1", Message: "m"}
	if got := e.Error(); got != "pgapi 400 E1: m" {
		t.Fatalf("Error() = %q", got)
	}
	e = &Error{StatusCode: 500}
	if got := e.Error(); got != "pgapi 500" {
		t.Fatalf("Error() = %q", got)
	}
	if !IsNotFound(&Error{StatusCode: http.StatusNotFound}) {
		t.Fatal("expected not found classification")
	}
	if !IsNotFound(fmt.Errorf("wrap: %w", &Error{StatusCode: http.StatusNotFound})) {
		t.Fatal("expected wrapped not found classification")
	}
	if IsNotFound(fmt.Errorf("plain")) {
		t.Fatal("plain error must not be not found")
	}
}

func writeEnvelope(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data, "message": "ok"})
}

func writeListEnvelope(w http.ResponseWriter, path string) {
	data := map[string]any{
		"items": []map[string]any{{"record_id": "rec1", "customer_name": "C", "contract_no": "HT-1"}},
		"page":  1, "page_size": 5, "total": 1,
		"pagination": map[string]any{"page": 1, "page_size": 5, "total": 1, "has_next": false},
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data, "message": "ok"})
}
