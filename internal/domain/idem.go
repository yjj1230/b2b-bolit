package domain

import (
	"fmt"
	"strings"
)

// ── 幂等键规范（L0-08，docs/02-architecture.html §5 三层幂等）──
//
// 键规范：
//   预订单  pre:<tenant>:<group>:<msgid>:<intent_version>
//   订单    order:<tenant>:<source>:<uuid>            (source = pre | quick)
//   发货    订单键 + ":ship:<seq>"
//   开票    订单键 + ":inv:<seq>"
//   补偿    业务键 + ":revert"
// 分段键派生自订单幂等键，保证「同订单的所有履约分录」可归并去重。

// ErrInvalidIdemKey 幂等键格式非法。
type ErrInvalidIdemKey struct{ Key, Reason string }

func (e *ErrInvalidIdemKey) Error() string {
	return fmt.Sprintf("invalid idempotency key %q: %s", e.Key, e.Reason)
}

// validateParts 校验键分段：非空、无非法字符（冒号分段符只允许出现在结构内）。
func validateParts(kind string, parts []string) error {
	if len(parts) < 2 {
		return &ErrInvalidIdemKey{Key: strings.Join(parts, ":"), Reason: "too few parts"}
	}
	if parts[0] != kind {
		return &ErrInvalidIdemKey{Key: strings.Join(parts, ":"), Reason: fmt.Sprintf("kind %q != %q", parts[0], kind)}
	}
	for _, p := range parts[1:] {
		if p == "" {
			return &ErrInvalidIdemKey{Key: strings.Join(parts, ":"), Reason: "empty part"}
		}
	}
	return nil
}

// PreOrderKey 构造预订单幂等键：pre:<tenant>:<group>:<msgid>:<intent_version>。
func PreOrderKey(tenant, group, msgID, intentVersion string) (string, error) {
	key := fmt.Sprintf("pre:%s:%s:%s:%s", tenant, group, msgID, intentVersion)
	if err := validateParts("pre", strings.Split(key, ":")); err != nil {
		return "", err
	}
	return key, nil
}

// OrderKey 构造订单幂等键：order:<tenant>:<source>:<uuid>（source = pre | quick）。
func OrderKey(tenant, source, uuid string) (string, error) {
	if source != "pre" && source != "quick" {
		return "", &ErrInvalidIdemKey{Key: source, Reason: "source must be pre|quick"}
	}
	key := fmt.Sprintf("order:%s:%s:%s", tenant, source, uuid)
	if err := validateParts("order", strings.Split(key, ":")); err != nil {
		return "", err
	}
	return key, nil
}

// SegmentKey 从订单幂等键派生分段键（发货 :ship:<seq> / 开票 :inv:<seq>）。
func SegmentKey(orderKey, segment string, seq int) (string, error) {
	if !strings.HasPrefix(orderKey, "order:") {
		return "", &ErrInvalidIdemKey{Key: orderKey, Reason: "not an order key"}
	}
	if segment != "ship" && segment != "inv" {
		return "", &ErrInvalidIdemKey{Key: segment, Reason: "segment must be ship|inv"}
	}
	return fmt.Sprintf("%s:%s:%d", orderKey, segment, seq), nil
}

// RevertKey 补偿幂等键：业务键 + ":revert"。
func RevertKey(businessKey string) string { return businessKey + ":revert" }

// ParsePreOrderKey 解析预订单幂等键，返回 (tenant, group, msgid, intentVersion)。
func ParsePreOrderKey(key string) (tenant, group, msgID, intentVersion string, err error) {
	parts := strings.Split(key, ":")
	if err = validateParts("pre", parts); err != nil {
		return
	}
	if len(parts) != 5 {
		err = &ErrInvalidIdemKey{Key: key, Reason: "pre key must have 5 parts"}
		return
	}
	return parts[1], parts[2], parts[3], parts[4], nil
}

// ParseOrderKey 解析订单幂等键，返回 (tenant, source, uuid)。
func ParseOrderKey(key string) (tenant, source, uuid string, err error) {
	parts := strings.Split(key, ":")
	if err = validateParts("order", parts); err != nil {
		return
	}
	if len(parts) != 4 {
		err = &ErrInvalidIdemKey{Key: key, Reason: "order key must have 4 parts"}
		return
	}
	return parts[1], parts[2], parts[3], nil
}
