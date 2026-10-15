package orderdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"b2b_pilot_v3/internal/pgapi"
)

func TestNewHTTPProviderValidation(t *testing.T) {
	if _, err := NewHTTPProvider("", "k"); err == nil {
		t.Fatal("expected empty base URL error")
	}
	if _, err := NewHTTPProvider("http://x", ""); err == nil {
		t.Fatal("expected empty api key error")
	}
	if _, err := NewHTTPProviderWithTimeout("http://x", "k", 3*time.Second); err != nil {
		t.Fatalf("WithTimeout: %v", err)
	}
	if p := NewHTTPProviderWithClient(nil); p == nil {
		t.Fatal("expected provider with custom client")
	}
}

func TestHTTPProviderMethods(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/db/customers/rec-c":
			writeJSON(w, envelope(map[string]any{"record_id": "rec-c", "customer_name": "客户C", "customer_level": "B"}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/db/products/rec-p":
			writeJSON(w, envelope(map[string]any{"record_id": "rec-p", "product_name": "产品P"}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/db/purchase-contracts/search":
			writeJSON(w, envelope(map[string]any{
				"items": []map[string]any{{"record_id": "rec-ct", "contract_no": "HT-1"}},
				"page":  1, "page_size": 10, "total": 1,
				"pagination": map[string]any{"page": 1, "page_size": 10, "total": 1, "has_next": false},
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/db/customers":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, envelope(map[string]any{"record_id": "rec-c2", "customer_name": "新客户"}))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/db/customers/rec-c2":
			writeJSON(w, envelope(map[string]any{"record_id": "rec-c2", "customer_name": "新客户", "customer_level": "A"}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/db/products":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, envelope(map[string]any{"record_id": "rec-p2", "product_name": "新产品"}))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/db/products/rec-p2":
			writeJSON(w, envelope(map[string]any{"record_id": "rec-p2", "product_name": "新产品", "spec": "25kg"}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	p, err := NewHTTPProvider(ts.URL, "k")
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	ctx := context.Background()

	grade, err := p.GetCustomerGrade(ctx, "chang-an", "rec-c")
	if err != nil || grade != "B" {
		t.Fatalf("GetCustomerGrade = %q err=%v", grade, err)
	}
	cust, err := p.GetCustomer(ctx, "chang-an", "rec-c")
	if err != nil || cust.CustomerName != "客户C" {
		t.Fatalf("GetCustomer: %+v err=%v", cust, err)
	}
	prod, err := p.GetProduct(ctx, "chang-an", "rec-p")
	if err != nil || prod.ProductName != "产品P" {
		t.Fatalf("GetProduct: %+v err=%v", prod, err)
	}
	cts, err := p.SearchPurchaseContracts(ctx, "chang-an", pgapi.Query{PageSize: 10})
	if err != nil || cts.Total != 1 || cts.Items[0].ContractNo != "HT-1" {
		t.Fatalf("SearchPurchaseContracts: %+v err=%v", cts, err)
	}

	nc, err := p.CreateCustomer(ctx, "chang-an", pgapi.CustomerWrite{CustomerName: "新客户", CustomerLevel: "B", CustomerType: "企业客户", Supplier: "供方"})
	if err != nil || nc.RecordID != "rec-c2" {
		t.Fatalf("CreateCustomer: %+v err=%v", nc, err)
	}
	level := "A"
	uc, err := p.UpdateCustomer(ctx, "chang-an", "rec-c2", pgapi.CustomerPatch{CustomerLevel: &level})
	if err != nil || uc.CustomerLevel != "A" {
		t.Fatalf("UpdateCustomer: %+v err=%v", uc, err)
	}
	np, err := p.CreateProduct(ctx, "chang-an", pgapi.ProductWrite{ProductName: "新产品", Spec: "20kg"})
	if err != nil || np.RecordID != "rec-p2" {
		t.Fatalf("CreateProduct: %+v err=%v", np, err)
	}
	spec := "25kg"
	up, err := p.UpdateProduct(ctx, "chang-an", "rec-p2", pgapi.ProductPatch{Spec: &spec})
	if err != nil || up.Spec != "25kg" {
		t.Fatalf("UpdateProduct: %+v err=%v", up, err)
	}
}

func TestHTTPProviderErrorPropagation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	p, err := NewHTTPProvider(ts.URL, "k")
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if _, err := p.GetCustomerGrade(context.Background(), "chang-an", "missing"); !pgapi.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func envelope(data any) map[string]any {
	return map[string]any{"success": true, "data": data, "message": "ok"}
}
