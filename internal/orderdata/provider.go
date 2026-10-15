// Package orderdata —— 订单数据实体访问边界（非流程相关数据）。
//
// 本包是 pgAPI 之上的唯一业务流程依赖面：
//   - HTTP/内存实现都在这里封装；
//   - workflows/api 只依赖 Provider 接口，不直接依赖 internal/pgapi 的 HTTP 细节；
//   - products/customers/purchase-contracts 的读写一律经 Provider，不直连 PostgreSQL。
package orderdata

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"b2b_pilot_v3/internal/pgapi"
)

// Provider 订单数据实体访问契约。
// tenantID 为业务租户参数；pgAPI 当前为单业务库，实现可忽略该参数或用于本地隔离。
type Provider interface {
	GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error)
	GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error)
	GetProduct(ctx context.Context, tenantID, productID string) (*pgapi.Product, error)
	SearchCustomers(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Customer], error)
	SearchProducts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.Product], error)
	SearchPurchaseContracts(ctx context.Context, tenantID string, q pgapi.Query) (*pgapi.ListResult[pgapi.PurchaseContract], error)

	// 写接口（D-006 Q3=B：接入 pgAPI POST/PATCH）
	CreateCustomer(ctx context.Context, tenantID string, in pgapi.CustomerWrite) (*pgapi.Customer, error)
	UpdateCustomer(ctx context.Context, tenantID, customerID string, in pgapi.CustomerPatch) (*pgapi.Customer, error)
	CreateProduct(ctx context.Context, tenantID string, in pgapi.ProductWrite) (*pgapi.Product, error)
	UpdateProduct(ctx context.Context, tenantID, productID string, in pgapi.ProductPatch) (*pgapi.Product, error)
}

// HTTPProvider pgAPI HTTP 实现。
type HTTPProvider struct {
	client *pgapi.Client
}

// NewHTTPProvider 构造 pgAPI Provider（默认 10s HTTP 超时）。
func NewHTTPProvider(baseURL, apiKey string) (*HTTPProvider, error) {
	return NewHTTPProviderWithTimeout(baseURL, apiKey, 10*time.Second)
}

// NewHTTPProviderWithTimeout 构造 pgAPI Provider 并指定 HTTP 超时。
func NewHTTPProviderWithTimeout(baseURL, apiKey string, timeout time.Duration) (*HTTPProvider, error) {
	httpClient := &http.Client{Timeout: timeout}
	client, err := pgapi.NewClient(baseURL, apiKey, httpClient)
	if err != nil {
		return nil, fmt.Errorf("orderdata: %w", err)
	}
	return &HTTPProvider{client: client}, nil
}

// NewHTTPProviderWithClient 注入定制 Client（测试用）。
func NewHTTPProviderWithClient(client *pgapi.Client) *HTTPProvider {
	return &HTTPProvider{client: client}
}

func (p *HTTPProvider) GetCustomerGrade(ctx context.Context, _ string, customerID string) (string, error) {
	c, err := p.client.GetCustomer(ctx, customerID)
	if err != nil {
		return "", err
	}
	return c.CustomerLevel, nil
}

func (p *HTTPProvider) GetCustomer(ctx context.Context, _ string, customerID string) (*pgapi.Customer, error) {
	return p.client.GetCustomer(ctx, customerID)
}

func (p *HTTPProvider) GetProduct(ctx context.Context, _ string, productID string) (*pgapi.Product, error) {
	return p.client.GetProduct(ctx, productID)
}

func (p *HTTPProvider) SearchCustomers(ctx context.Context, _ string, q pgapi.Query) (*pgapi.ListResult[pgapi.Customer], error) {
	return p.client.SearchCustomers(ctx, q)
}

func (p *HTTPProvider) SearchProducts(ctx context.Context, _ string, q pgapi.Query) (*pgapi.ListResult[pgapi.Product], error) {
	return p.client.SearchProducts(ctx, q)
}

func (p *HTTPProvider) SearchPurchaseContracts(ctx context.Context, _ string, q pgapi.Query) (*pgapi.ListResult[pgapi.PurchaseContract], error) {
	return p.client.SearchPurchaseContracts(ctx, q)
}

func (p *HTTPProvider) CreateCustomer(ctx context.Context, _ string, in pgapi.CustomerWrite) (*pgapi.Customer, error) {
	return p.client.CreateCustomer(ctx, in)
}

func (p *HTTPProvider) UpdateCustomer(ctx context.Context, _ string, customerID string, in pgapi.CustomerPatch) (*pgapi.Customer, error) {
	return p.client.UpdateCustomer(ctx, customerID, in)
}

func (p *HTTPProvider) CreateProduct(ctx context.Context, _ string, in pgapi.ProductWrite) (*pgapi.Product, error) {
	return p.client.CreateProduct(ctx, in)
}

func (p *HTTPProvider) UpdateProduct(ctx context.Context, _ string, productID string, in pgapi.ProductPatch) (*pgapi.Product, error) {
	return p.client.UpdateProduct(ctx, productID, in)
}

var _ Provider = (*HTTPProvider)(nil)
