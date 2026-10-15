package pgapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HeaderAPIKey pgAPI 鉴权请求头。
const HeaderAPIKey = "X-API-Key"

// Error pgAPI 调用错误（HTTP 层或业务失败响应）。
type Error struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("pgapi %d %s: %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("pgapi %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("pgapi %d", e.StatusCode)
	}
}

// IsNotFound 判断是否为 pgAPI 404（单资源不存在）。
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// Client pgAPI HTTP 客户端。
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient 构造客户端。httpClient 为 nil 时使用 10s 超时默认客户端。
func NewClient(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("pgapi: base URL is required")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("pgapi: API key is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, http: httpClient}, nil
}

// BaseURL 返回配置的服务地址（诊断/日志用）。
func (c *Client) BaseURL() string { return c.baseURL }

// ── 产品池 ──

// SearchProducts GET /api/db/products/search（GET /api/db/products 为等价别名）。
func (c *Client) SearchProducts(ctx context.Context, q Query) (*ListResult[Product], error) {
	return list[Product](c, ctx, "/api/db/products/search", q)
}

// GetProduct GET /api/db/products/{record_id}。
func (c *Client) GetProduct(ctx context.Context, recordID string) (*Product, error) {
	return get[Product](c, ctx, "/api/db/products/"+url.PathEscape(recordID))
}

// CreateProduct POST /api/db/products。
func (c *Client) CreateProduct(ctx context.Context, in ProductWrite) (*Product, error) {
	return write[Product](c, ctx, http.MethodPost, "/api/db/products", in)
}

// UpdateProduct PATCH /api/db/products/{record_id}。
func (c *Client) UpdateProduct(ctx context.Context, recordID string, in ProductPatch) (*Product, error) {
	return write[Product](c, ctx, http.MethodPatch, "/api/db/products/"+url.PathEscape(recordID), in)
}

// ── 客户池 ──

// SearchCustomers GET /api/db/customers/search（GET /api/db/customers 为等价别名）。
func (c *Client) SearchCustomers(ctx context.Context, q Query) (*ListResult[Customer], error) {
	return list[Customer](c, ctx, "/api/db/customers/search", q)
}

// GetCustomer GET /api/db/customers/{record_id}。
func (c *Client) GetCustomer(ctx context.Context, recordID string) (*Customer, error) {
	return get[Customer](c, ctx, "/api/db/customers/"+url.PathEscape(recordID))
}

// GetCustomerAddresses GET /api/db/customers/{record_id}/addresses。
func (c *Client) GetCustomerAddresses(ctx context.Context, recordID string) (*CustomerAddress, error) {
	return get[CustomerAddress](c, ctx, "/api/db/customers/"+url.PathEscape(recordID)+"/addresses")
}

// CreateCustomer POST /api/db/customers。
func (c *Client) CreateCustomer(ctx context.Context, in CustomerWrite) (*Customer, error) {
	return write[Customer](c, ctx, http.MethodPost, "/api/db/customers", in)
}

// UpdateCustomer PATCH /api/db/customers/{record_id}。
func (c *Client) UpdateCustomer(ctx context.Context, recordID string, in CustomerPatch) (*Customer, error) {
	return write[Customer](c, ctx, http.MethodPatch, "/api/db/customers/"+url.PathEscape(recordID), in)
}

// ── 购销合同（只读）──

// SearchPurchaseContracts GET /api/db/purchase-contracts/search。
func (c *Client) SearchPurchaseContracts(ctx context.Context, q Query) (*ListResult[PurchaseContract], error) {
	return list[PurchaseContract](c, ctx, "/api/db/purchase-contracts/search", q)
}

// GetPurchaseContract GET /api/db/purchase-contracts/{record_id}。
func (c *Client) GetPurchaseContract(ctx context.Context, recordID string) (*PurchaseContract, error) {
	return get[PurchaseContract](c, ctx, "/api/db/purchase-contracts/"+url.PathEscape(recordID))
}

// ── HTTP 通用路径 ──

type envelope[T any] struct {
	Success   bool   `json:"success"`
	Data      T      `json:"data"`
	Message   string `json:"message"`
	ErrorCode string `json:"error_code"`
	RequestID string `json:"request_id"`
	Detail    string `json:"detail"`
}

type listEnvelope[T any] struct {
	Success   bool        `json:"success"`
	Data      listData[T] `json:"data"`
	Message   string      `json:"message"`
	ErrorCode string      `json:"error_code"`
	RequestID string      `json:"request_id"`
	Detail    string      `json:"detail"`
}

type listData[T any] struct {
	Items      []T `json:"items"`
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	Pagination struct {
		Page     int  `json:"page"`
		PageSize int  `json:"page_size"`
		Total    int  `json:"total"`
		HasNext  bool `json:"has_next"`
	} `json:"pagination"`
	Sort struct {
		SortBy    string `json:"sort_by"`
		SortOrder string `json:"sort_order"`
	} `json:"sort"`
}

func list[T any](c *Client, ctx context.Context, path string, q Query) (*ListResult[T], error) {
	raw, _, err := c.do(ctx, http.MethodGet, path, buildQuery(q), nil)
	if err != nil {
		return nil, err
	}
	var env listEnvelope[T]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pgapi: decode list response: %w", err)
	}
	if err := responseError(env.Success, http.StatusOK, env.ErrorCode, env.Message, env.Detail, env.RequestID); err != nil {
		return nil, err
	}
	return &ListResult[T]{
		Items:    env.Data.Items,
		Page:     env.Data.Page,
		PageSize: env.Data.PageSize,
		Total:    env.Data.Total,
		HasNext:  env.Data.Pagination.HasNext,
	}, nil
}

func get[T any](c *Client, ctx context.Context, path string) (*T, error) {
	raw, _, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	var env envelope[T]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pgapi: decode response: %w", err)
	}
	if err := responseError(env.Success, http.StatusOK, env.ErrorCode, env.Message, env.Detail, env.RequestID); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

func write[T any](c *Client, ctx context.Context, method, path string, in any) (*T, error) {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("pgapi: marshal body: %w", err)
		}
	}
	raw, status, err := c.do(ctx, method, path, nil, body)
	if err != nil {
		return nil, err
	}
	var env envelope[T]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pgapi: decode write response: %w", err)
	}
	if err := responseError(env.Success, status, env.ErrorCode, env.Message, env.Detail, env.RequestID); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// do 发送请求并返回响应字节与 HTTP 状态。2xx 之外在 HTTP 层返回 *Error。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, int, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("pgapi: new request: %w", err)
	}
	req.Header.Set(HeaderAPIKey, c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("pgapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("pgapi: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, decodeHTTPError(resp.StatusCode, raw)
	}
	return raw, resp.StatusCode, nil
}

func decodeHTTPError(status int, raw []byte) *Error {
	var env envelope[json.RawMessage]
	_ = json.Unmarshal(raw, &env)
	msg := strings.TrimSpace(env.Message)
	if msg == "" {
		msg = strings.TrimSpace(env.Detail)
	}
	return &Error{StatusCode: status, Code: env.ErrorCode, Message: msg, RequestID: env.RequestID}
}

func responseError(success bool, status int, code, message, detail, requestID string) error {
	if success {
		return nil
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = strings.TrimSpace(detail)
	}
	return &Error{StatusCode: status, Code: code, Message: msg, RequestID: requestID}
}

func buildQuery(q Query) url.Values {
	v := url.Values{}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	v.Set("page", fmt.Sprint(page))
	v.Set("page_size", fmt.Sprint(pageSize))
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.SortBy != "" {
		v.Set("sort_by", q.SortBy)
	}
	if q.SortOrder != "" {
		v.Set("sort_order", q.SortOrder)
	}
	if q.IncludeDeleted {
		v.Set("include_deleted", "true")
	}
	for k, val := range q.Filters {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}
