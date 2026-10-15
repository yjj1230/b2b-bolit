package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
)

// L2-15 · 凭证链经 HTTP：追加版本 + 回放 + 权限拒绝（CQ-15，EP-40/EP-50）
func TestHTTPEvidenceChain(t *testing.T) {
	srv, _, store := newAPITest(t)
	h := srv.Routes()
	seedAPIWorld(t, store)
	orderID := createAPIOrder(t, h)
	store.SeedShipment("chang-an", orderID, "SHP-1", "wh-yangyuan", 40)

	// 正例 EP-50-P1/P3：uploader 追加 v1、v2
	rec := doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-1/evidence", orderID),
		`{"file_url":"s3://e/v1.png","file_hash":"h1"}`, map[string]string{"X-User-Id": "alice-sales"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var r1 struct{ Version int }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r1))
	require.Equal(t, 1, r1.Version)

	rec = doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-1/evidence", orderID),
		`{"file_url":"s3://e/v2.png","file_hash":"h2"}`, map[string]string{"X-User-Id": "alice-sales"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// 正例 EP-40-P3/EP-50-P2：回放全版本（有序）
	rec = doJSON(t, h, "GET", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-1/evidence", orderID), "",
		map[string]string{"X-User-Id": "alice-sales"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Versions []struct {
			Version  int    `json:"version"`
			FileHash string `json:"file_hash"`
		} `json:"versions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Versions, 2)
	require.Equal(t, 1, list.Versions[0].Version)
	require.Equal(t, "h2", list.Versions[1].FileHash)

	// 负例 EP-50-N2：无权限角色拒绝（CONTACT 无 append_evidence；独立 server 以 CONTACT 判定）
	srv2 := NewServer(store, nil, authz.NewStaticChecker(func(userID string) (authz.Role, error) {
		return authz.RoleContact, nil
	}), func(userID string) (authz.Role, string) { return authz.RoleContact, "chang-an" })
	h2 := srv2.Routes()
	rec = doJSON(t, h2, "POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-1/evidence", orderID),
		`{"file_url":"s3://e/v3.png","file_hash":"h3"}`, map[string]string{"X-User-Id": "carol-contact"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 负例：shipment 不属于该订单 → 404
	store.SeedShipment("chang-an", "ORD-OTHER", "SHP-9", "wh-yangyuan", 10)
	rec = doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-9/evidence", orderID),
		`{"file_url":"u","file_hash":"h"}`, map[string]string{"X-User-Id": "alice-sales"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 负例 EP-50-N3：缺 file_hash → 400
	rec = doJSON(t, h, "POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-1/evidence", orderID),
		`{"file_url":"u"}`, map[string]string{"X-User-Id": "alice-sales"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// L2-15b · 凭证权限矩阵：OP/WH/ADMIN 允许 append_evidence，CONTACT 拒绝
func TestCheckAppendEvidenceMatrix(t *testing.T) {
	check := func(role authz.Role) bool {
		ck := authz.NewStaticChecker(func(userID string) (authz.Role, error) { return role, nil })
		ok, err := ck.Check(t.Context(), authz.CheckRequest{
			Permission: authz.PermAppendEvidence, ResourceType: authz.ResShipment,
			ResourceID: "SHP-1", SubjectID: "u",
		})
		return err == nil && ok
	}
	for _, role := range []authz.Role{authz.RoleOP, authz.RoleWarehouse, authz.RoleAdmin} {
		if !check(role) {
			t.Errorf("role %s should allow append_evidence", role)
		}
	}
	for _, role := range []authz.Role{authz.RoleContact, authz.RoleSales, authz.RoleSupervisor} {
		if check(role) {
			t.Errorf("role %s should NOT allow append_evidence", role)
		}
	}
}
