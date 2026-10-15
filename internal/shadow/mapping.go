package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ── 事件 → Signal 映射（docs/v0.1/12 §6；与 config/shadow-feed.example.json 同构）──
//
// 三规则：
//   - 白名单推进：只投递映射表里已确认的动作；未知动作 → UNMAPPED_EVENT 分歧，绝不猜测；
//   - 幂等投递：(tenant_id, legacy_flow_id, legacy_event_id) 唯一；
//   - 事件时间序：同一 flow 按 occurred_at 串行投递，时间倒挂记录 DATA_GAP。

// SourceEvent 影子源事件（shadow_source_events 行）。
type SourceEvent struct {
	ID            int64
	TenantID      string
	LegacyFlowID  string
	LegacyEventID string
	StageCode     string
	Action        string
	Payload       map[string]any
	OccurredAt    time.Time
	Source        string
	Checksum      string
}

// FeedRule 单条映射规则：stage_code + action → signal（signal 空 = 影子建单 Start）。
type FeedRule struct {
	StageCode     string   `json:"stage_code"` // "*" 匹配任意段
	Action        string   `json:"action"`
	Signal        string   `json:"signal,omitempty"`
	Verdict       string   `json:"verdict,omitempty"`        // 固定裁决（如 APPROVED）
	VerdictField  string   `json:"verdict_field,omitempty"`  // 从 payload 字段取裁决
	PayloadFields []string `json:"payload_fields,omitempty"` // 复制进 Signal 载荷的字段
	Required      []string `json:"required,omitempty"`       // 必填 payload 字段（缺 → 不投递留痕）
	Note          string   `json:"note,omitempty"`           // 来源绑定：一条已确认的旧系统真实事件
}

// FeedConfig 映射配置（config/shadow-feed.example.json）。
type FeedConfig struct {
	TenantID string     `json:"tenant_id"`
	Rules    []FeedRule `json:"rules"`
	// 影子建单缺省值（源事件未携带时使用；真实映射在 T1.3 冻结权威版后覆盖）。
	DefaultProductID       string `json:"default_product_id"`
	DefaultUnitPriceCents  int64  `json:"default_unit_price_cents"`
	DefaultPaymentTerm     string `json:"default_payment_term"`
	DefaultSupplierChannel string `json:"default_supplier_channel"`
}

// LoadFeedConfig 读取并校验映射配置。
func LoadFeedConfig(path string) (*FeedConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read feed config: %w", err)
	}
	var c FeedConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse feed config: %w", err)
	}
	if c.TenantID == "" {
		return nil, fmt.Errorf("tenant_id required")
	}
	if len(c.Rules) == 0 {
		return nil, fmt.Errorf("rules required (白名单推进：无映射规则即拒绝投递)")
	}
	seen := map[string]bool{}
	for i, r := range c.Rules {
		if r.Action == "" {
			return nil, fmt.Errorf("rule[%d]: action required", i)
		}
		key := r.StageCode + "|" + r.Action
		if seen[key] {
			return nil, fmt.Errorf("rule[%d]: duplicate mapping %q", i, key)
		}
		seen[key] = true
	}
	return &c, nil
}

// Delivery 一条已翻译、可投递的影子动作。
type Delivery struct {
	Start   bool           `json:"start"` // 建影子订单 + StartWorkflow（每 flow 首个 ORD 事件）
	Signal  string         `json:"signal,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// ErrUnmapped 未映射动作（白名单推进：绝不猜测）。
type ErrUnmapped struct{ StageCode, Action string }

func (e *ErrUnmapped) Error() string {
	return fmt.Sprintf("unmapped event stage=%q action=%q", e.StageCode, e.Action)
}

// ErrMissingField 必填字段缺失（不投递并留痕）。
type ErrMissingField struct{ Field string }

func (e *ErrMissingField) Error() string { return "missing required payload field: " + e.Field }

// Translate S-02：源事件 → 影子动作。
// 已知动作正确翻译；未知动作返回 *ErrUnmapped；缺必填字段返回 *ErrMissingField。
func Translate(cfg *FeedConfig, ev SourceEvent) (Delivery, error) {
	for _, r := range cfg.Rules {
		if r.Action != ev.Action {
			continue
		}
		if r.StageCode != "*" && r.StageCode != "" && r.StageCode != ev.StageCode {
			continue
		}
		payload := map[string]any{}
		for _, f := range r.PayloadFields {
			if v, ok := ev.Payload[f]; ok {
				payload[f] = v
			}
		}
		for _, f := range r.Required {
			v, ok := payload[f]
			if !ok || v == nil || strings.TrimSpace(fmt.Sprint(v)) == "" {
				return Delivery{}, &ErrMissingField{Field: f}
			}
		}
		if r.VerdictField != "" {
			if v, ok := ev.Payload[r.VerdictField]; ok && v != nil {
				payload["verdict"] = v
			}
		} else if r.Verdict != "" {
			payload["verdict"] = r.Verdict
		}
		if r.Signal == "" || strings.EqualFold(r.Signal, "start") {
			return Delivery{Start: true, Payload: payload}, nil
		}
		if strings.EqualFold(r.Signal, "ignore") {
			// 显式忽略：已知但不驱动新系统状态机的旧系统状态（如 CHAIRMAN_SKIPPED、
			// CONTRACT_STUB_CREATED）。不投递、不写分歧，事件置 SKIPPED。
			return Delivery{Signal: "ignore", Payload: payload}, nil
		}
		return Delivery{Signal: r.Signal, Payload: payload}, nil
	}
	return Delivery{}, &ErrUnmapped{StageCode: ev.StageCode, Action: ev.Action}
}
