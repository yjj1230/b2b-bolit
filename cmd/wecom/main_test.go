package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

type robotCmdStore struct {
	groupNotFound bool
}

func (s *robotCmdStore) LookupWarehouseGroupBinding(ctx context.Context, groupID string) (string, string, error) {
	if s.groupNotFound {
		return "", "", &repo.ErrNotFound{What: "group " + groupID}
	}
	return "chang-an", "wh-yangyuan", nil
}

func (s *robotCmdStore) GetShipment(ctx context.Context, tenantID, shipmentID string) (*repo.Shipment, error) {
	return &repo.Shipment{ID: shipmentID, TenantID: tenantID, OrderID: "ORD-0112", WarehouseID: "wh-yangyuan"}, nil
}

func (s *robotCmdStore) AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta repo.EvidenceMeta) (int, error) {
	return 1, nil
}

// WEC-02 收敛：官方回调只有 MediaId 时回退 MediaID 作为识别引用；非仓库群静默忽略。
func TestHandleGroupImageMediaIDFallback(t *testing.T) {
	store := &robotCmdStore{}
	svc := &wecomService{robot: wecom.NewReceiptRobot(store)}

	rec := httptest.NewRecorder()
	svc.handleGroupImage(rec, httptest.NewRequest(http.MethodPost, "/wecom/callback/chang-an", nil), wecom.CallbackPayload{
		MsgType: "image", ChatID: "wr-group-1", MediaID: "ship-SHP-0112-receipt",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"recognized":true`)
	require.Contains(t, rec.Body.String(), "SHP-0112")

	store.groupNotFound = true
	rec = httptest.NewRecorder()
	svc.handleGroupImage(rec, httptest.NewRequest(http.MethodPost, "/wecom/callback/chang-an", nil), wecom.CallbackPayload{
		MsgType: "image", ChatID: "unknown-group", MediaID: "ship-SHP-x",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"ignored":true`)

	// robot disabled 分支
	svc2 := &wecomService{}
	rec = httptest.NewRecorder()
	svc2.handleGroupImage(rec, httptest.NewRequest(http.MethodPost, "/wecom/callback/chang-an", nil), wecom.CallbackPayload{MsgType: "image"})
	require.Contains(t, rec.Body.String(), "robot disabled")
}
