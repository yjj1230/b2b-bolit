package api

import (
	_ "embed"
	"net/http"
)

// bridgeUIHTML v0.1 极简 PC 下单后台（企微备案域名未就绪时的兜底入口）。
//
//go:embed bridge_ui.html
var bridgeUIHTML []byte

func (s *Server) handleBridgeUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(bridgeUIHTML)
}
