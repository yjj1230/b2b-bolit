package orderdata

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"b2b_pilot_v3/internal/pgapi"
)

// MemoryProvider 内存 Provider：本地演示与测试替身。
// 它不是生产数据源；生产使用 HTTPProvider。内存实现也提供写操作（POST/PATCH 语义），
// 便于在没有 pgAPI 服务的本地环境中跑通全部流程。
type MemoryProvider struct {
	mu        sync.Mutex
	seq       int
	customers map[string]*pgapi.Customer
	products  map[string]*pgapi.Product
	contracts map[string]*pgapi.PurchaseContract
}

// NewMemoryProvider 构造空内存 Provider。
func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{
		customers: map[string]*pgapi.Customer{},
		products:  map[string]*pgapi.Product{},
		contracts: map[string]*pgapi.PurchaseContract{},
	}
}

// NewDemoMemoryProvider 构造本地演示数据（与历史 demo 的 cust-0001/0002、
// prod-c68/prod-d90 对齐；这些值现在是 pgAPI record_id 的本地替身）。
func NewDemoMemoryProvider() *MemoryProvider {
	m := NewMemoryProvider()
	m.SeedCustomer("chang-an", "cust-0001", "北京华信科技", "A", "渠道A")
	m.SeedCustomer("chang-an", "cust-0002", "上海远景制造", "C", "渠道B")
	m.SeedProduct("chang-an", "prod-c68", "SKU-C68", "标准电机 C68")
	m.SeedProduct("chang-an", "prod-d90", "SKU-D90", "减速机 D90")
	m.SeedContract("chang-an", "rec-contract-demo", &pgapi.PurchaseContract{
		RecordID:     "rec-contract-demo",
		ApplyNo:      "202608170001",
		CustomerName: "北京华信科技",
		ProductName:  "标准电机 C68",
		ContractNo:   "HT-chang-an-demo",
		OrderStatus:  "已通过",
		SalesUser:    "张有连",
	})
	return m
}

// SeedCustomer 测试/演示注入客户。
func (m *MemoryProvider) SeedCustomer(tenantID, recordID, name, level, supplier string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customers[key(tenantID, recordID)] = &pgapi.Customer{
		RecordID: recordID, CustomerName: name, CustomerLevel: level, Supplier: supplier,
	}
}

// SeedProduct 测试/演示注入产品。
func (m *MemoryProvider) SeedProduct(tenantID, recordID, sku, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.products[key(tenantID, recordID)] = &pgapi.Product{
		RecordID: recordID, ProductName: name, Spec: sku,
	}
}

// SeedContract 测试/演示注入购销合同。
func (m *MemoryProvider) SeedContract(tenantID, recordID string, contract *pgapi.PurchaseContract) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *contract
	m.contracts[key(tenantID, recordID)] = &cp
}

func (m *MemoryProvider) GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error) {
	c, err := m.GetCustomer(ctx, tenantID, customerID)
	if err != nil {
		return "", err
	}
	return c.CustomerLevel, nil
}

func (m *MemoryProvider) GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.customers[key(tenantID, customerID)]; c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, &pgapi.Error{StatusCode: 404, Message: "customer not found"}
}

func (m *MemoryProvider) GetProduct(ctx context.Context, tenantID, productID string) (*pgapi.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.products[key(tenantID, productID)]; p != nil {
		cp := *p
		return &cp, nil
	}
	return nil, &pgapi.Error{StatusCode: 404, Message: "product not found"}
}

// SearchCustomers UI-01：客户搜索（q 匹配名称/编号）。
func (m *MemoryProvider) SearchCustomers(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Customer], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pgapi.Customer, 0)
	for k, c := range m.customers {
		if !hasTenantPrefix(k, tenantID) {
			continue
		}
		if q.Q != "" && !containsFold(c.CustomerName, q.Q) && !containsFold(c.CustomerNo, q.Q) {
			continue
		}
		if v := q.Filters["salesperson"]; v != "" && !hasSalesName(c.SalesUsers, v) {
			continue
		}
		out = append(out, *c)
	}
	return &pgapi.ListResult[pgapi.Customer]{Items: out, Page: 1, PageSize: maxInt(q.PageSize, 1), Total: len(out), HasNext: false}, nil
}

// SearchProducts UI-01：产品搜索（q 匹配名称/规格）。
func (m *MemoryProvider) SearchProducts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Product], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pgapi.Product, 0)
	for k, pr := range m.products {
		if !hasTenantPrefix(k, tenantID) {
			continue
		}
		if q.Q != "" && !containsFold(pr.ProductName, q.Q) && !containsFold(pr.Spec, q.Q) {
			continue
		}
		out = append(out, *pr)
	}
	return &pgapi.ListResult[pgapi.Product]{Items: out, Page: 1, PageSize: maxInt(q.PageSize, 1), Total: len(out), HasNext: false}, nil
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// hasSalesName 销售员姓名匹配（pgAPI salesperson 过滤的内存侧等价物）。
func hasSalesName(users []pgapi.SalesUser, name string) bool {
	for _, u := range users {
		if containsFold(u.Name, name) {
			return true
		}
	}
	return false
}

func (m *MemoryProvider) SearchPurchaseContracts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.PurchaseContract], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pgapi.PurchaseContract, 0, len(m.contracts))
	for k, c := range m.contracts {
		if !hasTenantPrefix(k, tenantID) {
			continue
		}
		out = append(out, *c)
	}
	return &pgapi.ListResult[pgapi.PurchaseContract]{Items: out, Page: 1, PageSize: maxInt(q.PageSize, 1), Total: len(out), HasNext: false}, nil
}

func (m *MemoryProvider) CreateCustomer(ctx context.Context, tenantID string, in pgapi.CustomerWrite) (*pgapi.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID("rec_cust")
	c := &pgapi.Customer{
		RecordID: id, CustomerNo: in.CustomerNo, CustomerName: in.CustomerName, Supplier: in.Supplier,
		SalesUsers: in.SalesUsers, Salesperson: in.Salesperson, CustomerLevel: in.CustomerLevel,
		CustomerType: in.CustomerType, TaxNo: in.TaxNo, InvoiceTaxNo: in.InvoiceTaxNo, Phone: in.Phone,
		Department: in.Department, CustomerNature: in.CustomerNature, RelationStatus: in.RelationStatus,
		ShippingAddress: in.ShippingAddress, Receiver: in.Receiver, RegisteredAddress: in.RegisteredAddress,
		BankName: in.BankName, BankAccount: in.BankAccount, PaymentTerms: in.PaymentTerms,
	}
	m.customers[key(tenantID, id)] = c
	return c, nil
}

func (m *MemoryProvider) UpdateCustomer(ctx context.Context, tenantID, customerID string, in pgapi.CustomerPatch) (*pgapi.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.customers[key(tenantID, customerID)]
	if c == nil {
		return nil, &pgapi.Error{StatusCode: 404, Message: "customer not found"}
	}
	applyString(&c.CustomerName, in.CustomerName)
	applyString(&c.CustomerLevel, in.CustomerLevel)
	applyString(&c.CustomerType, in.CustomerType)
	applyString(&c.Supplier, in.Supplier)
	applyString(&c.CustomerNo, in.CustomerNo)
	applyString(&c.TaxNo, in.TaxNo)
	applyString(&c.InvoiceTaxNo, in.InvoiceTaxNo)
	applyString(&c.Phone, in.Phone)
	if in.SalesUsers != nil {
		c.SalesUsers = in.SalesUsers
	}
	if in.Salesperson != nil {
		c.Salesperson = in.Salesperson
	}
	applyString(&c.Department, in.Department)
	applyString(&c.CustomerNature, in.CustomerNature)
	applyString(&c.RelationStatus, in.RelationStatus)
	applyString(&c.ShippingAddress, in.ShippingAddress)
	applyString(&c.Receiver, in.Receiver)
	applyString(&c.RegisteredAddress, in.RegisteredAddress)
	applyString(&c.BankName, in.BankName)
	applyString(&c.BankAccount, in.BankAccount)
	applyString(&c.PaymentTerms, in.PaymentTerms)
	return c, nil
}

func (m *MemoryProvider) CreateProduct(ctx context.Context, tenantID string, in pgapi.ProductWrite) (*pgapi.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID("rec_prod")
	isMortar, _ := json.Marshal(in.IsMortar)
	p := &pgapi.Product{
		RecordID: id, ProductName: in.ProductName, Spec: in.Spec, TechSpec: in.TechSpec,
		CategoryMajor: in.CategoryMajor, CategoryMinor: in.CategoryMinor, MajorType: in.MajorType,
		MinorType: in.MinorType, IsMortar: isMortar,
	}
	m.products[key(tenantID, id)] = p
	return p, nil
}

func (m *MemoryProvider) UpdateProduct(ctx context.Context, tenantID, productID string, in pgapi.ProductPatch) (*pgapi.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.products[key(tenantID, productID)]
	if p == nil {
		return nil, &pgapi.Error{StatusCode: 404, Message: "product not found"}
	}
	applyString(&p.ProductName, in.ProductName)
	applyString(&p.Spec, in.Spec)
	applyString(&p.TechSpec, in.TechSpec)
	applyString(&p.CategoryMajor, in.CategoryMajor)
	applyString(&p.CategoryMinor, in.CategoryMinor)
	applyString(&p.MajorType, in.MajorType)
	applyString(&p.MinorType, in.MinorType)
	if in.IsMortar != nil {
		if b, err := json.Marshal(in.IsMortar); err == nil {
			p.IsMortar = b
		}
	}
	return p, nil
}

func (m *MemoryProvider) nextID(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s_%04d", prefix, m.seq)
}

func key(parts ...string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += "/" + p
	}
	return out
}

func hasTenantPrefix(k, tenantID string) bool {
	return len(k) > len(tenantID) && k[:len(tenantID)+1] == tenantID+"/"
}

func applyString(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ Provider = (*MemoryProvider)(nil)
