package integration

import (
	"time"

	"b2b_pilot_v3/internal/domain"
)

// ── 业务配置性数据接口契约 ──────────────────────────────────────────────
//
// 文档出处（docs/01-core-graph.html §2、docs/02-architecture.html §2/§8.2）：
//   - 审批催办 24h 提醒 / 48h 升级   → ApprovalPolicy
//   - 无回单超 5 天自动例外审核       → ReceiptPolicy
//   - A/B/C/D/E 修改分类规则          → ModificationRulePolicy（01 §2.2："规则为配置数据"）
//   - 自动审批阈值（supervisor/chairman）→ AutoApprovePolicy（02 §8.2："不硬编码"）
//
// 当前基线实现为 StaticConfigProvider（static.go），值 = 历史硬编码默认值，
// 行为完全不变；后续可无缝替换为 DB/外部配置源实现（决策点：Static 起步）。

// ApprovalPolicy 审批催办策略（01 §2.1 Timer 催办升级：24h 提醒 / 48h 升级）。
type ApprovalPolicy interface {
	// RemindAfter 审批待办提醒间隔（默认 24h）。
	RemindAfter() time.Duration
	// EscalateAfter 审批升级间隔（默认 48h）。
	EscalateAfter() time.Duration
}

// ReceiptPolicy 无回单超期策略（01 §2.5 分流③：无回单超 5 天自动进例外审核）。
type ReceiptPolicy interface {
	// ReceiptExceptionAfter 无回单超期阈值（默认 5 天）。
	ReceiptExceptionAfter() time.Duration
}

// ModificationRulePolicy 修改分类规则（01 §2.2 A/B/C/D/E 分类规则表，配置数据）。
// 分类判定本身为纯函数（domain.ClassifyModification），本接口承载"规则可配置"的契约。
type ModificationRulePolicy interface {
	// Classify 判定修改分类（A/B/C/D/E），委托 domain 纯函数，规则来源可配置。
	Classify(m domain.Modification) domain.ModificationClass
	// TargetState 返回分类对应的 ORD 目标状态（与 OrdFSM 迁移表一致）。
	TargetState(class domain.ModificationClass) (string, error)
}

// AutoApprovePolicy 自动审批策略（02 §8.2：原系统 supervisor_auto_approve /
// chairman_auto_approve 迁移为审批策略配置数据，不硬编码）。
// 基线系统无自动审批（全部人工闸门，01 §2.1），默认实现恒返回 false。
type AutoApprovePolicy interface {
	// AutoApprove 判定指定审批阶段是否自动通过。
	AutoApprove(stageCode, grade string, amountCents int64) bool
}

// PreOrderPolicy 预订单确认升级策略（01 §2.6：24h 未处理 → Timer 升级）。
type PreOrderPolicy interface {
	// PreOrderEscalateAfter 预订单确认升级间隔（默认 24h）。
	PreOrderEscalateAfter() time.Duration
}

// BusinessConfigProvider 业务配置数据提供者聚合接口。
// 领域/流程层只依赖本接口，不感知 Static/DB/外部配置源的差异。
type BusinessConfigProvider interface {
	ApprovalPolicy
	ReceiptPolicy
	ModificationRulePolicy
	AutoApprovePolicy
	PreOrderPolicy
}
