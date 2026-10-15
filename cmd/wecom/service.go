//go:build !deferred

package main

import (
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
)

// ── 服务（默认构建：回调转接层；不含意图识别自动下单段）──

type wecomService struct {
	store   *repo.Repo
	gw      wecom.Gateway
	checker *authz.SpiceDBClient
	// handler 回调路由（07 §4.2）：验签后 EventKey → HTTP API 动作
	handler *wecom.CallbackHandler
	// robot 凭证机器人（0.1 A6：仓库群回单抓取识别登记）
	robot          *wecom.ReceiptRobot
	parser         wecom.IntentParser
	agentEnabled   bool
	ingressEnabled bool
	starter        preOrderStarter
}
