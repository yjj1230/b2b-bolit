package api

import (
	"encoding/json"
	"net/http"
)

// ── S1 侧边栏 H5 服务端端点（07 §6.1；对齐 v2 bff-api：/auth/exchange + /wecom/jsapi-config）──

// handleJSAPIConfig JS-SDK 双签名配置（wx.config 企业签名 + wx.agentConfig 应用签名）。
// GET /api/v1/wecom/jsapi-config?url=<当前页面 URL>；url 协议白名单由 builder 校验。
func (s *Server) handleJSAPIConfig(w http.ResponseWriter, r *http.Request) {
	if s.JSAPI == nil {
		writeErr(w, http.StatusNotImplemented, "jsapi config not enabled")
		return
	}
	cfg, err := s.JSAPI.BuildJSAPIConfig(r.Context(), r.URL.Query().Get("url"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleWeComOAuthURL 企微 snsapi_base 静默授权 URL（工作台 H5 OAuth 自举入口）。
// GET /api/v1/wecom/oauth-url?state=<state> → {"auth_url": "...", "strict": bool}，
// strict=true 表示生产严格模式（页面需自举 OAuth）；false=演示模式（保留 X-User-Id 流程，无需自举）。
func (s *Server) handleWeComOAuthURL(w http.ResponseWriter, r *http.Request) {
	if s.JSAPI == nil {
		writeErr(w, http.StatusNotImplemented, "jsapi config not enabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": s.JSAPI.OAuthURL(r.URL.Query().Get("state")),
		"strict":   s.AuthStrict,
	})
}

// handleExchange OAuth code + chat 会话 → 身份/上下文（服务端重新授权；07 §6.1
// 不信任前端传入身份）。响应含 context_token，后续请求经 X-WeCom-Context 回传并由
// withContext 校验签名与 principal 一致性。
// POST /api/v1/auth/exchange {"code": "...", "chat_id": "..."}
func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	if s.Identity == nil || s.Context == nil {
		writeErr(w, http.StatusNotImplemented, "wecom auth not enabled")
		return
	}
	var req struct {
		Code   string `json:"code"`
		ChatID string `json:"chat_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	ex, err := s.Identity.Exchange(r.Context(), req.Code, req.ChatID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := s.Context.Issue(ex.PrincipalID, ex.TenantID, ex.CustomerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue context: "+err.Error())
		return
	}
	ex.ContextToken = token
	writeJSON(w, http.StatusOK, ex)
}
