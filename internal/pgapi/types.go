// Package pgapi —— pgAPI（PostgreSQL 业务数据接口）HTTP 客户端。
//
// spec/pgAPI-接口文档.md v1.1：
//   - 服务地址 baseURL，例如 https://api.renheng.com/pgapi
//   - 所有业务请求携带 Header X-API-Key
//   - products/customers 支持查/增/改；purchase-contracts 只读
//
// 本包只负责 HTTP 协议与结构化类型，不包含业务流程；流程侧请经 internal/orderdata
// 的 Provider 接口依赖本包，避免把 HTTP 细节泄漏进 workflows/api。
package pgapi

import (
	"bytes"
	"encoding/json"
	"strings"
)

// SalesUser pgAPI 人员对象（sales_users / salesperson）。
type SalesUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Attachment 合同附件对象（ai_contract_upload）。
type Attachment struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	TmpURL    string `json:"tmp_url"`
	FileToken string `json:"file_token"`
}

// Product 产品池记录（字段按 spec §4）。
type Product struct {
	RecordID      string          `json:"record_id"`
	ProductName   string          `json:"product_name,omitempty"`
	Spec          string          `json:"spec,omitempty"`
	TechSpec      string          `json:"tech_spec,omitempty"`
	CategoryMajor string          `json:"category_major,omitempty"`
	CategoryMinor string          `json:"category_minor,omitempty"`
	MajorType     string          `json:"major_type,omitempty"`
	MinorType     string          `json:"minor_type,omitempty"`
	IsMortar      json.RawMessage `json:"is_mortar,omitempty"`
}

// ProductWrite POST /api/db/products 请求体。
// IsMortar 在查询示例中为字符串（"否"）、新增示例中为布尔值；用 any 透传。
type ProductWrite struct {
	ProductName   string `json:"product_name"`
	Spec          string `json:"spec"`
	TechSpec      string `json:"tech_spec,omitempty"`
	CategoryMajor string `json:"category_major,omitempty"`
	CategoryMinor string `json:"category_minor,omitempty"`
	MajorType     string `json:"major_type,omitempty"`
	MinorType     string `json:"minor_type,omitempty"`
	IsMortar      any    `json:"is_mortar,omitempty"`
}

// ProductPatch PATCH /api/db/products/{record_id} 请求体。
type ProductPatch struct {
	ProductName   *string `json:"product_name,omitempty"`
	Spec          *string `json:"spec,omitempty"`
	TechSpec      *string `json:"tech_spec,omitempty"`
	CategoryMajor *string `json:"category_major,omitempty"`
	CategoryMinor *string `json:"category_minor,omitempty"`
	MajorType     *string `json:"major_type,omitempty"`
	MinorType     *string `json:"minor_type,omitempty"`
	IsMortar      any     `json:"is_mortar,omitempty"`
}

// Customer 客户池记录（字段按 spec §5）。
type Customer struct {
	RecordID          string      `json:"record_id"`
	CustomerNo        string      `json:"customer_no,omitempty"`
	CustomerName      string      `json:"customer_name,omitempty"`
	Supplier          string      `json:"supplier,omitempty"`
	SalesUsers        []SalesUser `json:"sales_users,omitempty"`
	Salesperson       []SalesUser `json:"salesperson,omitempty"`
	CustomerLevel     string      `json:"customer_level,omitempty"`
	CustomerType      string      `json:"customer_type,omitempty"`
	TaxNo             string      `json:"tax_no,omitempty"`
	InvoiceTaxNo      string      `json:"invoice_tax_no,omitempty"`
	Phone             string      `json:"phone,omitempty"`
	Department        string      `json:"department,omitempty"`
	CustomerNature    string      `json:"customer_nature,omitempty"`
	RelationStatus    string      `json:"relation_status,omitempty"`
	ShippingAddress   string      `json:"shipping_address,omitempty"`
	Receiver          string      `json:"receiver,omitempty"`
	RegisteredAddress string      `json:"registered_address,omitempty"`
	BankName          string      `json:"bank_name,omitempty"`
	BankAccount       string      `json:"bank_account,omitempty"`
	PaymentTerms      string      `json:"payment_terms,omitempty"`
}

// UnmarshalJSON 兼容 pgAPI 客户返回的松散结构：department/payment_terms 等字段
// 可能是纯字符串，也可能是 [{text,type},...] 数组（或其它类型）；统一收敛为
// 逗号拼接的文本，避免因字段类型漂移导致整个列表解码失败。
func (c *Customer) UnmarshalJSON(b []byte) error {
	type rawCustomer struct {
		RecordID          string          `json:"record_id"`
		CustomerNo        json.RawMessage `json:"customer_no"`
		CustomerName      json.RawMessage `json:"customer_name"`
		Supplier          json.RawMessage `json:"supplier"`
		SalesUsers        []SalesUser     `json:"sales_users"`
		Salesperson       []SalesUser     `json:"salesperson"`
		CustomerLevel     json.RawMessage `json:"customer_level"`
		CustomerType      json.RawMessage `json:"customer_type"`
		TaxNo             json.RawMessage `json:"tax_no"`
		InvoiceTaxNo      json.RawMessage `json:"invoice_tax_no"`
		Phone             json.RawMessage `json:"phone"`
		Department        json.RawMessage `json:"department"`
		CustomerNature    json.RawMessage `json:"customer_nature"`
		RelationStatus    json.RawMessage `json:"relation_status"`
		ShippingAddress   json.RawMessage `json:"shipping_address"`
		Receiver          json.RawMessage `json:"receiver"`
		RegisteredAddress json.RawMessage `json:"registered_address"`
		BankName          json.RawMessage `json:"bank_name"`
		BankAccount       json.RawMessage `json:"bank_account"`
		PaymentTerms      json.RawMessage `json:"payment_terms"`
	}
	var r rawCustomer
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	c.RecordID = r.RecordID
	c.CustomerNo = rawToText(r.CustomerNo)
	c.CustomerName = rawToText(r.CustomerName)
	c.Supplier = rawToText(r.Supplier)
	c.SalesUsers = r.SalesUsers
	c.Salesperson = r.Salesperson
	c.CustomerLevel = rawToText(r.CustomerLevel)
	c.CustomerType = rawToText(r.CustomerType)
	c.TaxNo = rawToText(r.TaxNo)
	c.InvoiceTaxNo = rawToText(r.InvoiceTaxNo)
	c.Phone = rawToText(r.Phone)
	c.Department = rawToText(r.Department)
	c.CustomerNature = rawToText(r.CustomerNature)
	c.RelationStatus = rawToText(r.RelationStatus)
	c.ShippingAddress = rawToText(r.ShippingAddress)
	c.Receiver = rawToText(r.Receiver)
	c.RegisteredAddress = rawToText(r.RegisteredAddress)
	c.BankName = rawToText(r.BankName)
	c.BankAccount = rawToText(r.BankAccount)
	c.PaymentTerms = rawToText(r.PaymentTerms)
	return nil
}

// rawToText 把 pgAPI 返回的松散字段收敛为文本：字符串原样；[{text,...},...] 数组
// 取各元素的 text 逗号拼接；其它类型（数字/对象）返回紧凑 JSON 文本。
func rawToText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	var items []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err == nil {
		parts := make([]string, 0, len(items))
		for _, it := range items {
			if t := strings.TrimSpace(it.Text); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, ",")
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

// CustomerWrite POST /api/db/customers 请求体（必填四字段 + 常见可选字段）。
type CustomerWrite struct {
	CustomerName      string      `json:"customer_name"`
	CustomerLevel     string      `json:"customer_level"`
	CustomerType      string      `json:"customer_type"`
	Supplier          string      `json:"supplier"`
	CustomerNo        string      `json:"customer_no,omitempty"`
	TaxNo             string      `json:"tax_no,omitempty"`
	InvoiceTaxNo      string      `json:"invoice_tax_no,omitempty"`
	Phone             string      `json:"phone,omitempty"`
	SalesUsers        []SalesUser `json:"sales_users,omitempty"`
	Salesperson       []SalesUser `json:"salesperson,omitempty"`
	Department        string      `json:"department,omitempty"`
	CustomerNature    string      `json:"customer_nature,omitempty"`
	RelationStatus    string      `json:"relation_status,omitempty"`
	ShippingAddress   string      `json:"shipping_address,omitempty"`
	Receiver          string      `json:"receiver,omitempty"`
	RegisteredAddress string      `json:"registered_address,omitempty"`
	BankName          string      `json:"bank_name,omitempty"`
	BankAccount       string      `json:"bank_account,omitempty"`
	PaymentTerms      string      `json:"payment_terms,omitempty"`
}

// CustomerPatch PATCH /api/db/customers/{record_id} 请求体。
type CustomerPatch struct {
	CustomerName      *string     `json:"customer_name,omitempty"`
	CustomerLevel     *string     `json:"customer_level,omitempty"`
	CustomerType      *string     `json:"customer_type,omitempty"`
	Supplier          *string     `json:"supplier,omitempty"`
	CustomerNo        *string     `json:"customer_no,omitempty"`
	TaxNo             *string     `json:"tax_no,omitempty"`
	InvoiceTaxNo      *string     `json:"invoice_tax_no,omitempty"`
	Phone             *string     `json:"phone,omitempty"`
	SalesUsers        []SalesUser `json:"sales_users,omitempty"`
	Salesperson       []SalesUser `json:"salesperson,omitempty"`
	Department        *string     `json:"department,omitempty"`
	CustomerNature    *string     `json:"customer_nature,omitempty"`
	RelationStatus    *string     `json:"relation_status,omitempty"`
	ShippingAddress   *string     `json:"shipping_address,omitempty"`
	Receiver          *string     `json:"receiver,omitempty"`
	RegisteredAddress *string     `json:"registered_address,omitempty"`
	BankName          *string     `json:"bank_name,omitempty"`
	BankAccount       *string     `json:"bank_account,omitempty"`
	PaymentTerms      *string     `json:"payment_terms,omitempty"`
}

// CustomerAddress GET /api/db/customers/{record_id}/addresses 响应。
type CustomerAddress struct {
	RecordID          string `json:"record_id"`
	CustomerName      string `json:"customer_name,omitempty"`
	Receiver          string `json:"receiver,omitempty"`
	Phone             string `json:"phone,omitempty"`
	ShippingAddress   string `json:"shipping_address,omitempty"`
	RegisteredAddress string `json:"registered_address,omitempty"`
}

// PurchaseContract 购销合同记录（spec §6；只读）。
type PurchaseContract struct {
	RecordID         string       `json:"record_id"`
	Deleted          *bool        `json:"deleted,omitempty"`
	CreatedTime      *string      `json:"created_time,omitempty"`
	LastModifiedTime *string      `json:"last_modified_time,omitempty"`
	SyncedAt         string       `json:"synced_at,omitempty"`
	ApplyNo          string       `json:"apply_no,omitempty"`
	CustomerName     string       `json:"customer_name,omitempty"`
	Supplier         string       `json:"supplier,omitempty"`
	ProductName      string       `json:"product_name,omitempty"`
	Spec             []string     `json:"spec,omitempty"`
	QuantityKg       []float64    `json:"quantity_kg,omitempty"`
	UnitPrice        *float64     `json:"unit_price,omitempty"`
	TaxTotalAmount   *float64     `json:"tax_total_amount,omitempty"`
	ContractNo       string       `json:"contract_no,omitempty"`
	OrderStatus      string       `json:"order_status,omitempty"`
	ApprovalNode     *string      `json:"approval_node,omitempty"`
	ApprovalFlow     string       `json:"approval_flow,omitempty"`
	ShipStatus       []string     `json:"ship_status,omitempty"`
	StockStatus      *string      `json:"stock_status,omitempty"`
	InvoiceStatus    string       `json:"invoice_status,omitempty"`
	SalesUser        string       `json:"sales_user,omitempty"`
	Remark           *string      `json:"remark,omitempty"`
	SourceID         string       `json:"source_id,omitempty"`
	AIContractUpload []Attachment `json:"ai_contract_upload,omitempty"`
}

// UnmarshalJSON 兼容 pgAPI 合同返回的松散结构：spec/ship_status 可能是 [{text,type},...]
// 数组或内含 JSON 的字符串，quantity_kg 可能是标量；统一收敛为列表，避免整表解码失败。
func (pc *PurchaseContract) UnmarshalJSON(b []byte) error {
	type alias PurchaseContract
	var a struct {
		*alias
		Spec       json.RawMessage `json:"spec"`
		QuantityKg json.RawMessage `json:"quantity_kg"`
		ShipStatus json.RawMessage `json:"ship_status"`
	}
	a.alias = (*alias)(pc)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	pc.Spec = rawToTextList(a.Spec)
	pc.QuantityKg = rawToFloatList(a.QuantityKg)
	pc.ShipStatus = rawToTextList(a.ShipStatus)
	return nil
}

// rawToTextList 把 pgAPI 松散列表字段收敛为 []string：兼容 [{text,...},...] 数组、
// 纯字符串、以及内含 JSON 数组的字符串。
func rawToTextList(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		t := bytes.TrimSpace([]byte(s))
		if len(t) > 0 && (t[0] == '[' || t[0] == '{') {
			if items := rawToTextList(t); items != nil {
				return items
			}
		}
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var objs []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &objs); err == nil {
		out := make([]string, 0, len(objs))
		for _, it := range objs {
			if t := strings.TrimSpace(it.Text); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		return strs
	}
	if t := rawToText(raw); t != "" {
		return []string{t}
	}
	return nil
}

// rawToFloatList 把 quantity_kg 标量或数组统一收敛为 []float64。
func rawToFloatList(raw json.RawMessage) []float64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []float64
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &arr); err == nil {
			return arr
		}
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return []float64{f}
	}
	return nil
}

// ListResult 列表接口统一分页结果（业务侧只依赖这三个字段 + items）。
type ListResult[T any] struct {
	Items    []T  `json:"items"`
	Page     int  `json:"page"`
	PageSize int  `json:"page_size"`
	Total    int  `json:"total"`
	HasNext  bool `json:"has_next"`
}

// Query 通用列表查询参数（spec §3）。
type Query struct {
	Page           int
	PageSize       int
	Q              string
	Filters        map[string]string
	SortBy         string
	SortOrder      string
	IncludeDeleted bool
}
