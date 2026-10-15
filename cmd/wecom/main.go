// cmd/wecom —— 企微接入服务（docs/07-wecom-integration.html）
//
// 本期范围 = 企微 × 流程执行结合（07 §4.1「产品设计」）：
//   - 回调端点：URL 验证（GET）+ 消息回调（POST），验签后经 CallbackHandler
//     路由到 HTTP API 动作（审批/预订单确认——企微 = API 之上的转接层）
//   - 出站通知由 cmd/api（LocalRunner/Temporal worker 活动 → Notifier → Gateway）承担
//
// Agent 消息入口受 B2B_AGENT_ENABLED 和 B2B_WECOM_INGRESS_ENABLED 双开关保护；
// mock 入口仅用于测试，生产消息仍需经过已验签的企微通道。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"go.temporal.io/sdk/client"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	store, err := repo.New(ctx, cfg.PGDSN)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer store.Close()

	checker, err := authz.NewSpiceDB(cfg.SpiceDBAddr, cfg.SpiceDBKey)
	if err != nil {
		log.Fatalf("connect spicedb: %v", err)
	}

	gw, err := wecom.NewGateway(cfg)
	if err != nil {
		log.Fatalf("wecom gateway: %v", err)
	}
	log.Printf("wecom provider=%s", gw.Name())

	handler := &wecom.CallbackHandler{
		Actions: &wecom.HTTPActionClient{BaseURL: cfg.WeComAPIAddr, HTTP: &http.Client{Timeout: 10 * time.Second}},
		Tasks:   store,
		Logf:    log.Printf,
	}
	parser, err := buildIntentParser(cfg)
	if err != nil {
		log.Fatalf("configure agent parser: %v", err)
	}
	log.Printf("agent parser provider=%s model=%s", cfg.AgentParserProvider, cfg.AgentLLMModel)
	temporalClient, temporalErr := client.Dial(client.Options{HostPort: cfg.TemporalAddr, Namespace: cfg.TemporalNS})
	if temporalErr != nil {
		log.Printf("temporal unavailable; Agent ingress will remain manual: %v", temporalErr)
	}
	var starter preOrderStarter
	if temporalClient != nil {
		defer temporalClient.Close()
		starter = workflows.NewTemporalStarter(temporalClient, store)
	}
	svc := &wecomService{store: store, gw: gw, checker: checker, handler: handler,
		robot: wecom.NewReceiptRobot(store), parser: parser,
		agentEnabled: cfg.AgentEnabled, ingressEnabled: cfg.WeComIngressEnabled, starter: starter}

	mux := http.NewServeMux()
	registerRoutes(mux, svc, gw)

	addr := cfg.HTTPAddr
	log.Printf("wecom service listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// handleCallbackVerify 回调 URL 验证（GET；1 秒内响应 echostr 明文）。
func (s *wecomService) handleCallbackVerify(w http.ResponseWriter, r *http.Request) {
	echo, err := s.gw.VerifyURL(r.Context(), []byte(r.URL.RawQuery))
	if err != nil {
		writeErr(w, "verify url: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(echo)
}

// handleCallback 消息回调（POST；验签 → 按钮事件 → Signal 路由）。
func (s *wecomService) handleCallback(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0)
	if r.Body != nil {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
	}
	payload, err := s.gw.VerifyCallback(r.Context(), []byte(r.URL.RawQuery), body)
	if err != nil {
		writeErr(w, "verify callback: "+err.Error())
		return
	}
	// 0.1 A6：仓库群回单机器人——群图片消息先走机器人抓取登记（非仓库群静默忽略）
	if payload.MsgType == "image" && payload.ChatID != "" {
		s.handleGroupImage(w, r, payload)
		return
	}
	// 转接层路由（07 §4.2）：EventKey → HTTP API 动作；业务语义全部在 API 层。
	// 验签失败 → 400（W1）；动作失败（含 409 幂等回执）→ 502，企微侧可重试。
	if err := s.handler.Handle(r.Context(), payload); err != nil {
		writeJSONErr(w, http.StatusBadGateway, "callback action: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "event_key": payload.EventKey})
}

// handleGroupImage 仓库群回单机器人（A6/N-10）：抓取 → OCR 识别 → 匹配 → 登记 vN（只追加）。
// 官方回调：PicUrl→ImageURL、MediaId→MediaID；无 URL 时回退用 MediaID 作引用（真实 OCR 接线点）。
// 识别失败 → 200 recognized=false（人工补登）；非仓库群 → 静默忽略（不报错不打扰）。
func (s *wecomService) handleGroupImage(w http.ResponseWriter, r *http.Request, p wecom.CallbackPayload) {
	if s.robot == nil {
		writeJSON(w, map[string]any{"ok": true, "ignored": true, "reason": "robot disabled"})
		return
	}
	imageRef := p.ImageURL
	if imageRef == "" {
		imageRef = p.MediaID
	}
	res, err := s.robot.CaptureFromGroup(r.Context(), p.ChatID, imageRef, p.ImageHash)
	if err != nil {
		var nf *repo.ErrNotFound
		if errors.As(err, &nf) {
			// 非仓库群：静默忽略
			writeJSON(w, map[string]any{"ok": true, "ignored": true, "reason": "not warehouse group"})
			return
		}
		var rf *wecom.ErrRecognitionFailed
		if errors.As(err, &rf) {
			// 识别失败：人工补登（上传凭证 Tab 备选入口）
			writeJSON(w, map[string]any{"ok": true, "recognized": false, "reason": rf.Reason})
			return
		}
		writeJSONErr(w, http.StatusBadGateway, "receipt robot: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "recognized": true, "capture": res})
}

func writeErr(w http.ResponseWriter, msg string) {
	writeJSONErr(w, http.StatusBadRequest, msg)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
