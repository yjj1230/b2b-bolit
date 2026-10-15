package api

import (
	_ "embed"
	"net/http"
)

// assistantUIHTML v0.1 销售助理企微 H5 / PC 双端页面（嵌入部署）。
// 入口：
//
//	GET /assistant            工作台代下单（先选归属销售）
//	GET /assistant/?group_id=…&chat_id=…  客户群入口代下单（上下文锁定客户/归属）
//
// 页面只调用本项目 HTTP API：wecom jsapi-config / auth/exchange / bridge orders。
//
//go:embed assistant_ui.html
var assistantUIHTML []byte

func (s *Server) handleAssistantUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(assistantUIHTML)
}
