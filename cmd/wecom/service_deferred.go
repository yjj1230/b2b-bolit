//go:build deferred

package main

import (
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

// ── 服务（deferred 构建：含「意图识别自动下单」暂缓段）──

type wecomService struct {
	store   *repo.Repo
	gw      wecom.Gateway
	checker *authz.SpiceDBClient
	// handler 回调路由（07 §4.2）：验签后 EventKey → HTTP API 动作
	handler *wecom.CallbackHandler
	// parser 意图解析器：仅自动下单段使用（07 §3.0 暂缓实现、暂缓测试）。
	parser         wecom.IntentParser
	robot          *wecom.ReceiptRobot
	agentEnabled   bool
	ingressEnabled bool
	starter        preOrderStarter
}
