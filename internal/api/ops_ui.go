package api

import (
	_ "embed"
	"net/http"
)

// opsUIHTML v0.1 商务最小操作台（UI-02）：待办裁决 + 全量订单 + 发货单凭证登记。
// 仅调用本项目 HTTP API；生产入口 GET /ops。
//
//go:embed ops_ui.html
var opsUIHTML []byte

func (s *Server) handleOpsUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(opsUIHTML)
}
