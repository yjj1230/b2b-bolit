package orderdata

import (
	"context"
	"testing"

	"b2b_pilot_v3/internal/pgapi"
)

func TestMemoryProviderReadWrite(t *testing.T) {
	m := NewDemoMemoryProvider()
	ctx := context.Background()

	grade, err := m.GetCustomerGrade(ctx, "chang-an", "cust-0001")
	if err != nil || grade != "A" {
		t.Fatalf("grade = %q err=%v", grade, err)
	}
	if _, err := m.GetProduct(ctx, "chang-an", "prod-c68"); err != nil {
		t.Fatalf("product: %v", err)
	}
	if _, err := m.GetCustomer(ctx, "chang-an", "missing"); !pgapi.IsNotFound(err) {
		t.Fatalf("missing customer err=%v", err)
	}

	created, err := m.CreateCustomer(ctx, "chang-an", pgapi.CustomerWrite{
		CustomerName: "新客户", CustomerLevel: "B", CustomerType: "企业客户", Supplier: "供方",
	})
	if err != nil || created.RecordID == "" {
		t.Fatalf("create customer: %+v err=%v", created, err)
	}
	phone := "13800000000"
	updated, err := m.UpdateCustomer(ctx, "chang-an", created.RecordID, pgapi.CustomerPatch{Phone: &phone})
	if err != nil || updated.Phone != phone {
		t.Fatalf("update customer: %+v err=%v", updated, err)
	}

	prod, err := m.CreateProduct(ctx, "chang-an", pgapi.ProductWrite{ProductName: "新产品", Spec: "25kg"})
	if err != nil || prod.RecordID == "" {
		t.Fatalf("create product: %+v err=%v", prod, err)
	}
	spec := "20kg"
	updatedProd, err := m.UpdateProduct(ctx, "chang-an", prod.RecordID, pgapi.ProductPatch{Spec: &spec})
	if err != nil || updatedProd.Spec != spec {
		t.Fatalf("update product: %+v err=%v", updatedProd, err)
	}

	contracts, err := m.SearchPurchaseContracts(ctx, "chang-an", pgapi.Query{PageSize: 10})
	if err != nil || contracts.Total != 1 {
		t.Fatalf("contracts: %+v err=%v", contracts, err)
	}
}

// TestMemoryProviderSearch 覆盖 UI-01 客户/产品即输即搜与跨租户隔离。
func TestMemoryProviderSearch(t *testing.T) {
	m := NewDemoMemoryProvider()
	ctx := context.Background()

	m.SeedCustomer("other", "cust-9", "跨租户客户", "A", "供方")
	m.SeedProduct("other", "prod-9", "跨租户规格", "跨租户产品")

	custs, err := m.SearchCustomers(ctx, "chang-an", pgapi.Query{Q: "华信", PageSize: 50})
	if err != nil || custs.Total != 1 || custs.Items[0].RecordID != "cust-0001" {
		t.Fatalf("customers q: %+v err=%v", custs, err)
	}
	custs, err = m.SearchCustomers(ctx, "chang-an", pgapi.Query{PageSize: 50})
	if err != nil || custs.Total != 2 {
		t.Fatalf("customers all: %+v err=%v", custs, err)
	}
	custs, err = m.SearchCustomers(ctx, "other", pgapi.Query{PageSize: 50})
	if err != nil || custs.Total != 1 || custs.Items[0].RecordID != "cust-9" {
		t.Fatalf("cross-tenant customers: %+v err=%v", custs, err)
	}
	custs, err = m.SearchCustomers(ctx, "chang-an", pgapi.Query{Q: "不存在的客户"})
	if err != nil || custs.Total != 0 || len(custs.Items) != 0 {
		t.Fatalf("no match: %+v err=%v", custs, err)
	}

	prods, err := m.SearchProducts(ctx, "chang-an", pgapi.Query{Q: "C68"})
	if err != nil || prods.Total != 1 || prods.Items[0].RecordID != "prod-c68" {
		t.Fatalf("products q name: %+v err=%v", prods, err)
	}
	prods, err = m.SearchProducts(ctx, "chang-an", pgapi.Query{Q: "SKU-D90"})
	if err != nil || prods.Total != 1 || prods.Items[0].RecordID != "prod-d90" {
		t.Fatalf("products q spec: %+v err=%v", prods, err)
	}
	prods, err = m.SearchProducts(ctx, "other", pgapi.Query{})
	if err != nil || prods.Total != 1 || prods.Items[0].RecordID != "prod-9" {
		t.Fatalf("cross-tenant products: %+v err=%v", prods, err)
	}

	contracts, err := m.SearchPurchaseContracts(ctx, "other", pgapi.Query{})
	if err != nil || contracts.Total != 0 {
		t.Fatalf("cross-tenant contracts: %+v err=%v", contracts, err)
	}

	if !containsFold("ABC", "abc") || containsFold("abc", "zzz") {
		t.Fatalf("containsFold behavior wrong")
	}
}
