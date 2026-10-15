//go:build !deferred

package main

import (
	"context"
	"net/http"

	"b2b_pilot_v3/internal/wecom"
)

// registerRoutes 默认构建路由：回调、健康检查以及受开关保护的 Agent 测试注入/会话存档入口。
func registerRoutes(mux *http.ServeMux, svc *wecomService, gw wecom.Gateway) {
	mux.HandleFunc("GET /wecom/callback/{tenant}", svc.handleCallbackVerify)
	mux.HandleFunc("POST /wecom/callback/{tenant}", svc.handleCallback)
	mux.HandleFunc("GET /wecom/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "provider": gw.Name()})
	})
	// Mock 仅用于测试注入；真实入口由官方回调/会话存档配置控制。
	mux.HandleFunc("POST /wecom/mock/message", svc.handleMockMessage)
	if svc.agentEnabled && svc.ingressEnabled && gw.Name() == "official" {
		go svc.archiveLoop(context.Background())
	}
}
