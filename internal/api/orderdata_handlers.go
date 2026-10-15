package api

import (
	"net/http"
	"strconv"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
)

// ── UI-01 收敛：订单数据实体搜索端点（pgAPI 只读，供 assistant_ui/bridge_ui 预填）──
// v0.1 商务（OP/ADMIN）专属；未配置 pgAPI Provider 时 501（页面降级为手工填写）。

func (s *Server) dataProviderOr501(w http.ResponseWriter, r *http.Request) (orderdata.Provider, bool) {
	if s.Data == nil {
		writeErr(w, http.StatusNotImplemented, "orderdata provider not configured (pgAPI)")
		return nil, false
	}
	u := userOf(r.Context())
	if u.Role != string(authz.RoleOP) && u.Role != string(authz.RoleAdmin) {
		writeErr(w, http.StatusForbidden, "orderdata search is ops-only")
		return nil, false
	}
	return s.Data, true
}

func dataQuery(r *http.Request) pgapi.Query {
	q := pgapi.Query{Q: r.URL.Query().Get("q"), Filters: map[string]string{}}
	for _, k := range []string{"grade", "customer_name", "product_name", "customer_id", "salesperson"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Filters[k] = v
		}
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			q.PageSize = n
		}
	}
	return q
}

func (s *Server) handleSearchCustomers(w http.ResponseWriter, r *http.Request) {
	data, ok := s.dataProviderOr501(w, r)
	if !ok {
		return
	}
	res, err := data.SearchCustomers(r.Context(), userOf(r.Context()).TenantID, dataQuery(r))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if res == nil {
		res = &pgapi.ListResult[pgapi.Customer]{Items: []pgapi.Customer{}}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSearchProducts(w http.ResponseWriter, r *http.Request) {
	data, ok := s.dataProviderOr501(w, r)
	if !ok {
		return
	}
	res, err := data.SearchProducts(r.Context(), userOf(r.Context()).TenantID, dataQuery(r))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if res == nil {
		res = &pgapi.ListResult[pgapi.Product]{Items: []pgapi.Product{}}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSearchContracts(w http.ResponseWriter, r *http.Request) {
	data, ok := s.dataProviderOr501(w, r)
	if !ok {
		return
	}
	res, err := data.SearchPurchaseContracts(r.Context(), userOf(r.Context()).TenantID, dataQuery(r))
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if res == nil {
		res = &pgapi.ListResult[pgapi.PurchaseContract]{Items: []pgapi.PurchaseContract{}}
	}
	writeJSON(w, http.StatusOK, res)
}
