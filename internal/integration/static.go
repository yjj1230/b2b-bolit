package integration

import (
	"time"

	"b2b_pilot_v3/internal/domain"
)

// ── StaticConfigProvider：默认基线实现 ──────────────────────────────────
//
// 值 = 历史硬编码默认值（docs/01-core-graph.html §2 与代码历史常量），
// 保证行为完全不变（决策点：Static 起步）。后续可新增 DBConfigProvider /
// 外部配置源实现，业务代码零改动。

// DefaultApprovalRemindAfter 审批待办提醒默认值（24h，01 §2.1）。
const DefaultApprovalRemindAfter = 24 * time.Hour

// DefaultApprovalEscalateAfter 审批升级默认值（48h，01 §2.1）。
const DefaultApprovalEscalateAfter = 48 * time.Hour

// DefaultReceiptExceptionAfter 无回单超期默认值（5 天，01 §2.5）。
const DefaultReceiptExceptionAfter = 5 * 24 * time.Hour

// DefaultPreOrderEscalateAfter 预订单确认升级默认值（24h，01 §2.6）。
const DefaultPreOrderEscalateAfter = 24 * time.Hour

// StaticConfigProvider 静态配置实现（值固定，测试/基线环境默认）。
type StaticConfigProvider struct {
	approvalRemindAfter   time.Duration
	approvalEscalateAfter time.Duration
	receiptExceptionAfter time.Duration
	preOrderEscalateAfter time.Duration
}

// NewStaticConfigProvider 构造基线默认值（= 历史硬编码常量，行为不变）。
func NewStaticConfigProvider() *StaticConfigProvider {
	return &StaticConfigProvider{
		approvalRemindAfter:   DefaultApprovalRemindAfter,
		approvalEscalateAfter: DefaultApprovalEscalateAfter,
		receiptExceptionAfter: DefaultReceiptExceptionAfter,
		preOrderEscalateAfter: DefaultPreOrderEscalateAfter,
	}
}

// RemindAfter 实现 ApprovalPolicy。
func (s *StaticConfigProvider) RemindAfter() time.Duration { return s.approvalRemindAfter }

// EscalateAfter 实现 ApprovalPolicy。
func (s *StaticConfigProvider) EscalateAfter() time.Duration { return s.approvalEscalateAfter }

// ReceiptExceptionAfter 实现 ReceiptPolicy。
func (s *StaticConfigProvider) ReceiptExceptionAfter() time.Duration { return s.receiptExceptionAfter }

// Classify 实现 ModificationRulePolicy（委托 domain 纯函数，规则值可配置化）。
func (s *StaticConfigProvider) Classify(m domain.Modification) domain.ModificationClass {
	return domain.ClassifyModification(m)
}

// TargetState 实现 ModificationRulePolicy。
func (s *StaticConfigProvider) TargetState(class domain.ModificationClass) (string, error) {
	return domain.ModifyTargetState(class)
}

// AutoApprove 实现 AutoApprovePolicy。基线无自动审批（全部人工闸门），恒 false。
func (s *StaticConfigProvider) AutoApprove(stageCode, grade string, amountCents int64) bool {
	return false
}

// PreOrderEscalateAfter 实现 PreOrderPolicy。
func (s *StaticConfigProvider) PreOrderEscalateAfter() time.Duration { return s.preOrderEscalateAfter }

// 编译期断言：StaticConfigProvider 满足 BusinessConfigProvider。
var _ BusinessConfigProvider = (*StaticConfigProvider)(nil)
