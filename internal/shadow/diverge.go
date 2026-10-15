// Package shadow —— 影子全流程预演核心（docs/v0.1/12-shadow-full-flow.html）
//
// 纯函数域：FSM 回放与边覆盖率（S-01）、事件→Signal 映射（S-02）、
// 分歧四分类（S-03）、feed 投递计划（幂等 + 时间序，S-05）；
// 另有 pgx 影子仓储（008 五表）供 shadow-ingest/feed/audit 使用。
package shadow

import "fmt"

// 分歧四分类（12 §9.1：发现 → 分类 → 裁决 → 修复 → 重放验证）。
const (
	KindMapping       = "MAPPING"        // 旧系统事件语义映射错 / 字段取错
	KindFSMGap        = "FSM_GAP"        // 真实业务合法但我们的 FSM 拒绝（最值钱发现）
	KindLegacyAnomaly = "LEGACY_ANOMALY" // 飞书侧数据断链 / 非法迁移 / 缺事件
	KindDataGap       = "DATA_GAP"       // 同步源缺事件 / 前缀缺失 / 乱序倒挂
)

// 分歧具体原因（写入 detail / shadow_divergences.detail）。
const (
	ReasonUnmappedEvent      = "UNMAPPED_EVENT"
	ReasonMissingField       = "MISSING_FIELD"
	ReasonIllegalTransition  = "ILLEGAL_TRANSITION"
	ReasonTimeInversion      = "TIME_INVERSION"
	ReasonNoCreationEvent    = "NO_CREATION_EVENT"
	ReasonStuckWaitingSignal = "STUCK_WAITING_SIGNAL"
	ReasonStateMismatch      = "STATE_MISMATCH"
	ReasonChainBreak         = "CHAIN_BREAK"
)

// 严重度。
const (
	SeverityP0 = "P0"
	SeverityP1 = "P1"
	SeverityP2 = "P2"
)

// 状态。
const (
	DivergenceOpen    = "OPEN"
	DivergenceDecided = "DECIDED"
	DivergenceClosed  = "CLOSED"
)

// Divergence 影子分歧记录（12 §8.1 shadow_divergences 的业务模型）。
type Divergence struct {
	Kind         string `json:"kind"`
	Severity     string `json:"severity"`
	Reason       string `json:"reason"`
	LegacyFlowID string `json:"legacy_flow_id,omitempty"`
	OrderID      string `json:"order_id,omitempty"`
	FromState    string `json:"from_state,omitempty"`
	ToState      string `json:"to_state,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Status       string `json:"status"`
	Owner        string `json:"owner,omitempty"`
	// SourceEventID 触发分歧的源事件（feed 用于把该事件置 SKIPPED，避免每轮重复分歧）。
	SourceEventID int64 `json:"source_event_id,omitempty"`
}

// NewDivergence 构造分歧；未给 severity 时按四分类给默认档。
// FSM_GAP / LEGACY_ANOMALY 默认 P1（阻塞 v0.2 决策）；MAPPING / DATA_GAP 默认 P2。
func NewDivergence(kind, severity, reason, flowID, detail string) Divergence {
	if severity == "" {
		switch kind {
		case KindFSMGap, KindLegacyAnomaly:
			severity = SeverityP1
		default:
			severity = SeverityP2
		}
	}
	return Divergence{
		Kind: kind, Severity: severity, Reason: reason, LegacyFlowID: flowID,
		Detail: detail, Status: DivergenceOpen,
	}
}

// ClassifyDivergence S-03：分歧四分类判定纯函数。
// 输入观察到的异常类别，返回四分类与建议处置；未知类别保守归入 LEGACY_ANOMALY（登记待裁决）。
func ClassifyDivergence(reason string) (kind, severity, action string) {
	switch reason {
	case ReasonUnmappedEvent, ReasonMissingField:
		return KindMapping, SeverityP2, "改 shadow-feed.json 映射后重放该 flow"
	case ReasonIllegalTransition:
		return KindFSMGap, SeverityP1, "改 domain/workflow + 补 L0-L3 测试，全量重放"
	case ReasonTimeInversion, ReasonNoCreationEvent:
		return KindDataGap, SeverityP2, "补同步源 / 标前缀缺口，重放后复检"
	case ReasonChainBreak, ReasonStateMismatch, ReasonStuckWaitingSignal:
		return KindLegacyAnomaly, SeverityP1, "不修旧系统：登记 v0.3 回填前清洗/裁决清单"
	default:
		return KindLegacyAnomaly, SeverityP1, "登记裁决：未知分歧保守进入 v0.3 清洗清单"
	}
}

// ValidateDivergence 校验分歧记录完整性。
func ValidateDivergence(d Divergence) error {
	if d.Kind == "" {
		return fmt.Errorf("divergence kind required")
	}
	switch d.Kind {
	case KindMapping, KindFSMGap, KindLegacyAnomaly, KindDataGap:
	default:
		return fmt.Errorf("unknown divergence kind %q", d.Kind)
	}
	if d.Severity != SeverityP0 && d.Severity != SeverityP1 && d.Severity != SeverityP2 {
		return fmt.Errorf("unknown severity %q", d.Severity)
	}
	return nil
}
