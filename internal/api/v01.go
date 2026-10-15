package api

import (
	"context"
	"encoding/json"
	"net/http"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

// ── 0.1 发布版新增端点（docs/v0.1/08-api.html）──
//  P8  发群守门员（A3/N-01）：POST /api/v1/orders/{orderID}/send-to-group
//  P9  多单合并开票追加关联（A4/N-05）：POST /api/v1/invoices/{invoiceID}/link-orders
//  P10 订单三金额分展（A4）：GET /api/v1/orders/{orderID}/invoice-summary
// 仓储能力经窄接口类型断言（repo.Repo 实现；fake/降级实现缺能力时 501）。

// gatekeeperStore 守门员确认事件仓储契约（repo.Repo 实现）。
type gatekeeperStore interface {
	AppendGatekeeperConfirmation(ctx context.Context, tenantID, orderID, contentHash string, detail []byte, operator string) (eventID int64, already bool, err error)
}

// invoiceLinkStore 发票-订单关联仓储契约（repo.Repo 实现）。
type invoiceLinkStore interface {
	LinkInvoiceOrder(ctx context.Context, tenantID, invoiceID, orderID string, amountCents, qty int64) (bool, error)
	ListInvoiceLinks(ctx context.Context, tenantID, invoiceID string) ([]repo.InvoiceOrderLink, error)
	GetInvoice(ctx context.Context, tenantID, invoiceID string) (*repo.Invoice, error)
}

// threeAmountStore 订单三金额仓储契约（repo.Repo 实现）。
type threeAmountStore interface {
	OrderThreeAmounts(ctx context.Context, tenantID, orderID string) (*repo.OrderThreeAmounts, error)
}

// shipmentAmountStore 发货单金额仓储契约（repo.Repo 实现；N-06 凭证金额守恒）。
type shipmentAmountStore interface {
	ShipmentShippedAmountCents(ctx context.Context, tenantID, shipmentID string) (int64, error)
}

// evidenceMetaStore 凭证识别元数据仓储契约（repo.Repo 实现；A2 主路径 + A6 机器人）。
type evidenceMetaStore interface {
	AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta repo.EvidenceMeta) (int, error)
}

// SendToGroupRequest 发群请求（守门员）。
type SendToGroupRequest struct {
	GroupID string `json:"group_id"`
	Kind    string `json:"kind"`    // order_card / logistics_image / invoice
	Content string `json:"content"` // 发群内容（卡片文案/图片描述）
	Confirm bool   `json:"confirm"` // 守门员二次确认：false=仅询问；true=确认发送
}

// handleSendToGroup 发群守门员（A3/N-01）：
//  1. 授权：销售（唯一确认角色）且对订单有 edit 权限；其余 403；
//  2. confirm=false → 不发送、不记录确认事件，返回 confirmation_required；
//  3. confirm=true → 幂等确认（同订单同内容指纹只确认一次）→ 记录
//     GatekeeperConfirmed 事件（append-only 审计）→ 出站群发；
//  4. 重复确认 → 返回既有确认记录（already_confirmed），不重复发送。
func (s *Server) handleSendToGroup(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	// 销售是守门员唯一确认角色（05 §N8）；ADMIN 演示模式放行
	if u.Role != string(authz.RoleSales) && u.Role != string(authz.RoleAdmin) {
		writeErr(w, http.StatusForbidden, "gatekeeper confirm is sales-only")
		return
	}
	if _, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	ok, err := s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermEdit, ResourceType: authz.ResOrder, ResourceID: orderID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	var req SendToGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := domain.ValidateGatekeeperContent(req.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	contentHash := domain.GatekeeperContentHash(req.Content)
	if !req.Confirm {
		// 未确认：不发送、不记录（弹守门员确认由前端完成）
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "confirmation_required": true, "content_hash": contentHash,
		})
		return
	}
	gs, okStore := s.Store.(gatekeeperStore)
	if !okStore {
		writeErr(w, http.StatusNotImplemented, "gatekeeper store not supported")
		return
	}
	detail := repo.MarshalJSONDetail(map[string]any{
		"content_hash": contentHash, "group_id": req.GroupID, "kind": req.Kind, "content": req.Content,
	})
	eventID, already, err := gs.AppendGatekeeperConfirmation(r.Context(), u.TenantID, orderID, contentHash, detail, u.UserID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	if already {
		// 幂等回执：确认只发生一次，不重复发送
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "already_confirmed": true, "event_id": eventID, "sent": false,
		})
		return
	}
	sent := false
	fallback := ""
	if s.GroupSender != nil && req.GroupID != "" {
		if _, err := s.GroupSender.SendGroupCard(r.Context(), u.TenantID, req.GroupID, wecom.Card{
			TenantID: u.TenantID, OrderID: orderID, Kind: "GATEKEEPER_SEND", State: req.Kind,
		}); err != nil {
			// WEC-04 收敛：官方群发不可达不把守门员确认打成失败——确认已落事件链，
			// 返回可读回退指令（PC 兜底/人工转发），现场按 staff_scene §6.3 裁决。
			fallback = "群发失败（" + err.Error() + "）：请改用 PC 兜底（/bridge）或销售人工转发并记录裁决"
		} else {
			sent = true
		}
	} else {
		fallback = "群发通道未配置：请改用 PC 兜底（/bridge）或销售人工转发并记录裁决"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "already_confirmed": false, "event_id": eventID, "sent": sent,
		"fallback_required": sent == false, "fallback": fallback,
	})
}

// LinkInvoiceOrderRequest 追加发票-订单关联请求（A4：漏了可补关联第 3 笔）。
type LinkInvoiceOrderRequest struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Qty         int64  `json:"qty"`
}

// handleLinkInvoiceOrder 多单合并开票追加关联（A4/N-05）：
// 授权 order.edit → 订单级守恒（金额/数量 ≤ 未开票额度）→ 幂等追加。
func (s *Server) handleLinkInvoiceOrder(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	invoiceID := r.PathValue("invoiceID")
	ils, ok := s.Store.(invoiceLinkStore)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "invoice link store not supported")
		return
	}
	inv, err := ils.GetInvoice(r.Context(), u.TenantID, invoiceID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	var req LinkInvoiceOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.OrderID == "" || req.AmountCents <= 0 || req.Qty <= 0 {
		writeErr(w, http.StatusBadRequest, "order_id and positive amount_cents/qty required")
		return
	}
	ok, err = s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermEdit, ResourceType: authz.ResOrder, ResourceID: req.OrderID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	created, err := ils.LinkInvoiceOrder(r.Context(), u.TenantID, invoiceID, req.OrderID, req.AmountCents, req.Qty)
	_ = created
	if err != nil {
		if domain.IsConservation(err) {
			writeErr(w, http.StatusConflict, "超出未开票额度: "+err.Error())
			return
		}
		writeErr(w, errStatus(err), err.Error())
		return
	}
	links, err := ils.ListInvoiceLinks(r.Context(), u.TenantID, invoiceID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "created": created, "invoice_id": invoiceID,
		"invoice_state": inv.State, "links": links,
	})
}

// handleInvoiceSummary 订单三金额分展（A4：收款/发货/开票 + 未开票额度保留在订单上）。
func (s *Server) handleInvoiceSummary(w http.ResponseWriter, r *http.Request) {
	u := userOf(r.Context())
	orderID := r.PathValue("orderID")
	if _, err := s.Store.GetOrder(r.Context(), u.TenantID, orderID); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	ok, err := s.Checker.Check(r.Context(), authz.CheckRequest{
		Permission: authz.PermView, ResourceType: authz.ResOrder, ResourceID: orderID, SubjectID: u.UserID,
	})
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "permission denied")
		return
	}
	tas, okStore := s.Store.(threeAmountStore)
	if !okStore {
		writeErr(w, http.StatusNotImplemented, "three-amount store not supported")
		return
	}
	amounts, err := tas.OrderThreeAmounts(r.Context(), u.TenantID, orderID)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	// 仓管剥价（CQ-03：金额不可见）
	if u.Role == string(authz.RoleWarehouse) {
		writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "amounts_hidden": true})
		return
	}
	writeJSON(w, http.StatusOK, amounts)
}
