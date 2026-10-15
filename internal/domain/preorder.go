package domain

// PreOrderFSM —— 预订单流程状态机（docs/01-core-graph.html §2.6，企微 S2 核心）。
//
// AI 安全边界：任何生成结果都需人工确认（PENDING_CONFIRM → CONFIRMED/MODIFIED）
// 才能进入 ORDER_REQUESTED；低置信只发轻提醒（AMBIGUOUS），不自动建单。
var PreOrderFSM = NewFSM("PRE", StatePreCaptured, map[string][]string{
	StatePreCaptured: {
		StatePreCollecting,    // 首条消息进入多轮收集
		StatePreIntentParsing, // 意图识别（LLM 只产候选 + 确定性校验）
	},
	StatePreIntentParsing: {
		StatePreCollecting,
		StatePrePendingConfirm, // 高置信且校验通过
		StatePreAmbiguous,      // 低置信/多候选/高金额（AI 安全边界）
		StatePreIgnored,        // 发送者权限校验失败（07 §3.2：忽略 + 审计）
	},
	StatePreCollecting: {
		StatePreCollecting,     // 吸收新消息并生成新草稿版本
		StatePrePendingConfirm, // 必填字段完整且策略允许确认
		StatePreAmbiguous,      // 会话或字段冲突
		StatePreEscalated,      // 达到提醒/升级时间
		StatePreExpired,        // 达到硬过期时间
		StatePreIgnored,        // 权限失败或人工忽略
	},
	StatePreAmbiguous: {
		StatePreCollecting,
		StatePrePendingConfirm, // 轻提醒后销售修正确认
		StatePreIgnored,        // 销售忽略
		StatePreExpired,
	},
	StatePrePendingConfirm: {
		StatePreCollecting, // 用户补充消息使旧卡片失效
		StatePreConfirmed,  // ✅ 销售确认（卡片按钮→Signal）
		StatePreModified,   // 销售修改后确认
		StatePreIgnored,    // 销售忽略
		StatePreEscalated,  // 24h 未处理→Timer 升级
		StatePreExpired,    // 硬过期
	},
	StatePreConfirmed: {
		StatePreOrderRequested, // 生成正式订单（订单幂等键）
	},
	StatePreModified: {
		StatePreOrderRequested, // 生成正式订单（修改后参数）
	},
	StatePreEscalated: {
		StatePreCollecting,
		StatePrePendingConfirm, // 升级后继续等待确认
		StatePreExpired,
	},
	StatePreExpired: {},
})
