package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"b2b_pilot_v3/internal/bridge"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
)

// ── v0.1 销售助手下单桥接端点（docs/发布计划v2 第 1 次发布）──
// 三个端点对应 A1 方案：授权 URL 生成、OAuth code 回调换 token（服务端缓存）、下单调用器。

func (s *Server) handleBridgeAuthURL(w http.ResponseWriter, r *http.Request) {
	if s.Bridge == nil {
		writeErr(w, http.StatusServiceUnavailable, "feishu bridge not enabled")
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "v01"
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": s.Bridge.AuthorizationURL(state)})
}

func (s *Server) handleBridgeCallback(w http.ResponseWriter, r *http.Request) {
	if s.Bridge == nil {
		writeErr(w, http.StatusServiceUnavailable, "feishu bridge not enabled")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
		writeErr(w, http.StatusBadRequest, "code required")
		return
	}
	user := userOf(r.Context())
	res, err := s.Bridge.ExchangeCode(r.Context(), user.UserID, in.Code)
	if err != nil {
		if bridge.IsAuthExpired(err) {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identity":           res.Identity,
		"expires_in_seconds": 2 * 3600,
		"reauth_window_secs": 15 * 60,
	})
}

func (s *Server) handleBridgeCreateOrder(w http.ResponseWriter, r *http.Request) {
	if s.Bridge == nil {
		writeErr(w, http.StatusServiceUnavailable, "feishu bridge not enabled")
		return
	}
	var in bridgeOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.OrderID == "" {
		in.OrderID = newBridgeOrderID()
	}
	if in.Currency != "" && in.Currency != "CNY" && in.Currency != "USD" && in.Currency != "EUR" {
		writeErr(w, http.StatusBadRequest, "currency must be CNY, USD or EUR")
		return
	}
	user := userOf(r.Context())
	token, _, ok := s.Bridge.TokenFor(user.UserID)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "feishu authorization required: GET /api/v1/bridge/auth-url first")
		return
	}
	// T1.3/HR2-2c：老客户 ID 交叉映射——新系统 pgAPI record_id(api_*) ≠ 旧系统 bitable record_id(rec*)。
	// 提交前把非 rec 的客户 ID 按 客户名+销售员 解析为旧系统 rec id，否则旧系统归属校验查不到档案。
	if in.CustomerRecordID != "" && !strings.HasPrefix(in.CustomerRecordID, "rec") {
		rec, rerr := s.Bridge.ResolveExistingCustomerRecord(r.Context(), token, in.CustomerName, in.SalesOpenID)
		if rerr != nil {
			writeErr(w, http.StatusBadGateway, "resolve customer record: "+rerr.Error())
			return
		}
		if rec == "" {
			writeErr(w, http.StatusBadRequest, "客户档案未在旧系统找到（请改为新客户或重新选择老客户）")
			return
		}
		in.CustomerRecordID = rec
	}
	items := make([]bridge.Item, 0, len(in.Items))
	for _, it := range in.Items {
		unitPrice := it.UnitPrice
		if unitPrice == 0 {
			unitPrice = centsToYuan(it.UnitPriceCents)
		}
		items = append(items, bridge.Item{
			ProductName: it.ProductName,
			Quantity:    it.Quantity,
			UnitPrice:   unitPrice,
			Packaging:   it.Packaging,
			PackageSpec: it.PackageSpec,
			ArrivalDate: it.ArrivalDate,
			Remark:      legacyItemRemark(it.Remark, it.PackingRemark, it.QualityReportRequired),
		})
	}
	sub, err := s.Bridge.CreateOrder(r.Context(), token, bridge.OrderInput{
		TenantID:           user.TenantID,
		OrderID:            in.OrderID,
		CustomerRecordID:   in.CustomerRecordID,
		CustomerName:       in.CustomerName,
		CustomerLevel:      in.CustomerLevel,
		TaxNo:              in.TaxNo,
		Supplier:           in.Supplier,
		Items:              items,
		PaymentTerms:       in.PaymentTerms,
		RemittanceMethod:   in.RemittanceMethod,
		Currency:           in.Currency,
		ShippingAddress:    in.ShippingAddress,
		Receiver:           in.Receiver,
		Phone:              in.Phone,
		IsSelfPickup:       in.IsSelfPickup,
		PickupWarehouse:    in.PickupWarehouse,
		CustomerContractNo: in.CustomerContractNo,
		OrderRemark:        in.OrderRemark,
		SpecialSpec:        in.SpecialSpec,
		Sales: bridge.Identity{
			Name:   in.SalesName,
			OpenID: in.SalesOpenID,
			UserID: in.SalesUserID,
			Role:   in.SalesRole,
		},
		Agent: bridge.Identity{
			Name:   in.AgentName,
			OpenID: in.AgentOpenID,
			UserID: in.AgentUserID,
			Role:   in.AgentRole,
		},
	})
	if err != nil {
		if bridge.IsAuthExpired(err) {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sub)
	// 新客户（未关联老客户档案）：测试特化——best-effort 把客户档案落备库 pgAPI（幂等查重，失败仅记日志不阻断下单）。
	// 备库无同步机制 → 写入持久保留，老客户按销售员搜索即可命中。
	// 开关 B2B_PGAPI_ARCHIVE_ENABLED（生产默认关）：生产走「新客户写多维表 → rhcrm-sync → PG → 老客户读取」原链路，不直写。
	if in.CustomerRecordID == "" && s.Data != nil && s.PGAPIArchiveEnabled {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			s.ensurePgapiCustomerArchive(ctx, user.TenantID, in)
		}()
	}
}

type bridgeOrderRequest struct {
	OrderID          string `json:"order_id"`
	CustomerRecordID string `json:"customer_record_id"`
	CustomerName     string `json:"customer_name"`
	CustomerLevel    string `json:"customer_level"`
	TaxNo            string `json:"tax_no"`
	Supplier         string `json:"supplier"`
	Items            []struct {
		ProductName           string  `json:"product_name"`
		Quantity              float64 `json:"quantity"`
		UnitPrice             float64 `json:"unit_price"`
		UnitPriceCents        int64   `json:"unit_price_cents"`
		Packaging             string  `json:"packaging"`
		PackageSpec           string  `json:"package_spec"`
		ArrivalDate           string  `json:"arrival_date"`
		Remark                string  `json:"remark"`
		PackingRemark         string  `json:"packing_remark"`
		QualityReportRequired bool    `json:"quality_report_required"`
	} `json:"items"`
	PaymentTerms       string `json:"payment_terms"`
	RemittanceMethod   string `json:"remittance_method"`
	Currency           string `json:"currency"`
	ShippingAddress    string `json:"shipping_address"`
	Receiver           string `json:"receiver"`
	Phone              string `json:"phone"`
	IsSelfPickup       bool   `json:"is_self_pickup"`
	PickupWarehouse    string `json:"pickup_warehouse"`
	CustomerContractNo string `json:"customer_contract_no"`
	OrderRemark        string `json:"order_remark"`
	SpecialSpec        string `json:"special_spec_requirements"`
	SalesName          string `json:"sales_name"`
	SalesOpenID        string `json:"sales_open_id"`
	SalesUserID        string `json:"sales_user_id"`
	SalesRole          string `json:"sales_role_key"`
	AgentName          string `json:"agent_name"`
	AgentOpenID        string `json:"agent_open_id"`
	AgentUserID        string `json:"agent_user_id"`
	AgentRole          string `json:"agent_role_key"`
}

// handleBridgeSalesPersons 代理旧系统 /sales-persons：返回当前代办可选归属销售列表（含 user_id/open_id）。
func (s *Server) handleBridgeSalesPersons(w http.ResponseWriter, r *http.Request) {
	if s.Bridge == nil {
		if s.LocalSalesPersons == nil {
			writeErr(w, http.StatusServiceUnavailable, "sales persons provider not enabled")
			return
		}
		items, err := s.LocalSalesPersons.ListSalesPersons(r.Context(), userOf(r.Context()).TenantID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if items == nil {
			items = []bridge.SalesPerson{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	user := userOf(r.Context())
	token, _, ok := s.Bridge.TokenFor(user.UserID)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "feishu authorization required: GET /api/v1/bridge/auth-url first")
		return
	}
	items, err := s.Bridge.ListSalesPersons(r.Context(), token)
	if err != nil {
		if bridge.IsAuthExpired(err) {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if items == nil {
		items = []bridge.SalesPerson{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleBridgeListOrders(w http.ResponseWriter, r *http.Request) {
	if s.BridgeLedger == nil {
		writeErr(w, http.StatusServiceUnavailable, "bridge ledger not enabled")
		return
	}
	user := userOf(r.Context())
	items, err := s.BridgeLedger.ListBridgeSubmissions(r.Context(), user.TenantID, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []repo.BridgeSubmission{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func centsToYuan(cents int64) float64 {
	return float64(cents) / 100
}

// legacyItemRemark 把新版表单字段收敛到旧系统已支持的单条商品备注，
// 不扩张 external-orders/create 的 items_json 契约。
func legacyItemRemark(remark, packingRemark string, qualityReportRequired bool) string {
	parts := make([]string, 0, 3)
	for _, value := range []string{strings.TrimSpace(packingRemark), strings.TrimSpace(remark)} {
		if value != "" && (len(parts) == 0 || parts[len(parts)-1] != value) {
			parts = append(parts, value)
		}
	}
	if qualityReportRequired {
		parts = append(parts, "需要质检报告")
	}
	return strings.Join(parts, "；")
}

func newBridgeOrderID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "BRG-" + hex.EncodeToString(b)
}

// ensurePgapiCustomerArchive 新客户档案落备库 pgAPI：按名称幂等查重（已存在则跳过）；
// customer_name/supplier 为 pgAPI 必填字段，缺失则跳过；创建失败仅记日志（best-effort）。
func (s *Server) ensurePgapiCustomerArchive(ctx context.Context, tenantID string, in bridgeOrderRequest) {
	if in.CustomerName == "" || in.Supplier == "" {
		s.Logf("pgapi archive skip: customer_name/supplier empty for %q", in.CustomerName)
		return
	}
	level := in.CustomerLevel
	if level == "" {
		level = "A"
	}
	if existing, err := s.Data.SearchCustomers(ctx, tenantID, pgapi.Query{Q: in.CustomerName, PageSize: 20}); err == nil {
		for _, c := range existing.Items {
			if strings.EqualFold(strings.TrimSpace(c.CustomerName), strings.TrimSpace(in.CustomerName)) {
				s.Logf("pgapi archive skip: %q already exists", in.CustomerName)
				return
			}
		}
	}
	write := pgapi.CustomerWrite{
		CustomerName:    in.CustomerName,
		CustomerLevel:   level,
		CustomerType:    "新客户",
		Supplier:        in.Supplier,
		TaxNo:           in.TaxNo,
		Phone:           in.Phone,
		ShippingAddress: in.ShippingAddress,
		Receiver:        in.Receiver,
		PaymentTerms:    in.PaymentTerms,
	}
	// 关联销售员（姓名+open_id）：新客户落备库后即可按销售员过滤命中（测试按姓名，生产按 open_id 精确）。
	if in.SalesName != "" || in.SalesOpenID != "" {
		su := pgapi.SalesUser{ID: in.SalesOpenID, Name: in.SalesName}
		write.SalesUsers = []pgapi.SalesUser{su}
		write.Salesperson = []pgapi.SalesUser{su}
	}
	if _, err := s.Data.CreateCustomer(ctx, tenantID, write); err != nil {
		s.Logf("pgapi archive create failed for %q: %v", in.CustomerName, err)
	}
}
