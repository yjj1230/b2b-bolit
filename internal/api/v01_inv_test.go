package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/workflows"
)

// invNoStore 在 FakeStore 之外补充 INV-01 需要的发票号重复检查。
type invNoStore struct {
	*workflows.FakeStore
	seen map[string]bool
}

func (s *invNoStore) InvoiceNoExists(ctx context.Context, tenantID, invoiceNo string) (bool, error) {
	return s.seen[invoiceNo], nil
}

// taxProvider 提供客户档案税号（INV-01 抬头校验）。
type taxProvider struct {
	*orderdata.MemoryProvider
	tax string
}

func (p *taxProvider) GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error) {
	return &pgapi.Customer{RecordID: customerID, TaxNo: p.tax}, nil
}

// errInvStore 覆盖 InvoiceNoExists 错误分支。
type errInvStore struct {
	*workflows.FakeStore
}

func (s *errInvStore) InvoiceNoExists(ctx context.Context, tenantID, invoiceNo string) (bool, error) {
	return false, errors.New("db down")
}

// errTaxProvider 覆盖 pgAPI 客户档案查询错误分支。
type errTaxProvider struct{ *orderdata.MemoryProvider }

func (p *errTaxProvider) GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error) {
	return nil, errors.New("pgapi down")
}

// INV-01 收敛：发票号必填 + 重复发票 409 + 抬头税号一致 + 金额守恒（已有）。
func TestInvoiceEvidenceHardChecks(t *testing.T) {
	base := workflows.NewFakeStoreForTest()
	store := &invNoStore{FakeStore: base, seen: map[string]bool{"NO-DUP": true}}
	starter := newFakeStarter(store)
	checker := authz.NewStaticChecker(func(string) (authz.Role, error) { return authz.RoleAccounting, nil })
	srv := NewServer(store, starter, checker, func(userID string) (authz.Role, string) {
		if userID == "u-op" {
			return authz.RoleOP, "chang-an"
		}
		return authz.RoleAccounting, "chang-an"
	})
	srv.Data = &taxProvider{MemoryProvider: orderdata.NewMemoryProvider(), tax: "91110000XXXXXXXX1A"}
	h := srv.Routes()

	seedAPIWorld(t, base)
	orderID := createAPIOrder(t, h)
	base.SeedPrepared("chang-an", orderID, "item-1", 10)
	base.SeedShipment("chang-an", orderID, "SHP-inv1", "wh-yangyuan", 10)
	shp := base.ShipmentFor("chang-an", "SHP-inv1")
	shp.State = domain.ShipmentStateShipped
	shp.ShippedQty = 10
	shp.PlannedQty = 10
	path := "/api/v1/orders/" + orderID + "/shipments/SHP-inv1/evidence"

	// 缺 invoice_no → 400
	rec := doJSON(t, h, http.MethodPost, path,
		`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","amount_cents":1000}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 重复发票号 → 409
	rec = doJSON(t, h, http.MethodPost, path,
		`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-DUP","amount_cents":1000}`, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// 税号不一致 → 400
	rec = doJSON(t, h, http.MethodPost, path,
		`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-NEW","buyer_tax_no":"WRONG","amount_cents":1000}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 全部通过 → 201
	rec = doJSON(t, h, http.MethodPost, path,
		`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-NEW","buyer_tax_no":"91110000XXXXXXXX1A","amount_cents":1000}`, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// INV-01 分支补强：无 pgAPI 时跳过税号比对；发票查重失败与 pgAPI 查询失败可读错误。
func TestInvoiceEvidenceInv01Branches(t *testing.T) {
	seed := func(t *testing.T) (*workflows.FakeStore, *invNoStore) {
		base := workflows.NewFakeStoreForTest()
		store := &invNoStore{FakeStore: base, seen: map[string]bool{}}
		seedAPIWorld(t, base)
		return base, store
	}

	t.Run("no data provider", func(t *testing.T) {
		base, store := seed(t)
		srv := NewServer(store, newFakeStarter(store), authz.NewStaticChecker(func(string) (authz.Role, error) { return authz.RoleAccounting, nil }),
			func(userID string) (authz.Role, string) {
				if userID == "u-op" {
					return authz.RoleOP, "chang-an"
				}
				return authz.RoleAccounting, "chang-an"
			})
		h := srv.Routes()
		orderID := createAPIOrder(t, h)
		base.SeedShipment("chang-an", orderID, "SHP-b1", "wh-yangyuan", 10)
		shp := base.ShipmentFor("chang-an", "SHP-b1")
		shp.State = domain.ShipmentStateShipped
		shp.ShippedQty = 10
		shp.PlannedQty = 10
		rec := doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/shipments/SHP-b1/evidence",
			`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-X","buyer_tax_no":"WHATEVER","amount_cents":1000}`, nil)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("invoice duplicate check error", func(t *testing.T) {
		base, _ := seed(t)
		store := &errInvStore{FakeStore: base}
		srv := NewServer(store, newFakeStarter(store), authz.NewStaticChecker(func(string) (authz.Role, error) { return authz.RoleAccounting, nil }),
			func(userID string) (authz.Role, string) {
				if userID == "u-op" {
					return authz.RoleOP, "chang-an"
				}
				return authz.RoleAccounting, "chang-an"
			})
		h := srv.Routes()
		orderID := createAPIOrder(t, h)
		base.SeedShipment("chang-an", orderID, "SHP-b2", "wh-yangyuan", 10)
		shp := base.ShipmentFor("chang-an", "SHP-b2")
		shp.State = domain.ShipmentStateShipped
		shp.ShippedQty = 10
		shp.PlannedQty = 10
		rec := doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/shipments/SHP-b2/evidence",
			`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-X","amount_cents":1000}`, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("pgapi tax lookup error", func(t *testing.T) {
		base, store := seed(t)
		srv := NewServer(store, newFakeStarter(store), authz.NewStaticChecker(func(string) (authz.Role, error) { return authz.RoleAccounting, nil }),
			func(userID string) (authz.Role, string) {
				if userID == "u-op" {
					return authz.RoleOP, "chang-an"
				}
				return authz.RoleAccounting, "chang-an"
			})
		srv.Data = &errTaxProvider{MemoryProvider: orderdata.NewMemoryProvider()}
		h := srv.Routes()
		orderID := createAPIOrder(t, h)
		base.SeedShipment("chang-an", orderID, "SHP-b3", "wh-yangyuan", 10)
		shp := base.ShipmentFor("chang-an", "SHP-b3")
		shp.State = domain.ShipmentStateShipped
		shp.ShippedQty = 10
		shp.PlannedQty = 10
		rec := doJSON(t, h, http.MethodPost, "/api/v1/orders/"+orderID+"/shipments/SHP-b3/evidence",
			`{"file_url":"f1","file_hash":"h1","kind":"INVOICE","invoice_no":"NO-X","buyer_tax_no":"WHATEVER","amount_cents":1000}`, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}

func TestFirstNonEmptyStr(t *testing.T) {
	require.Equal(t, "", firstNonEmptyStr("", "", ""))
	require.Equal(t, "b", firstNonEmptyStr("", "b", "c"))
}
