package bridge

import "fmt"

// ErrorKind 桥接错误分类（对应旧系统 externalOrders.ts 错误码语义）。
type ErrorKind string

const (
	ErrAuthExpired          ErrorKind = "FEISHU_AUTH_EXPIRED"
	ErrOrderDataInvalid     ErrorKind = "ORDER_DATA_INVALID"
	ErrServiceMisconfigured ErrorKind = "SERVICE_MISCONFIGURED"
	ErrIdempotencyConflict  ErrorKind = "IDEMPOTENCY_CONFLICT"
	ErrUnknown              ErrorKind = "BRIDGE_ERROR"
)

// Error 桥接错误。
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "bridge: " + string(e.Kind)
	}
	return fmt.Sprintf("bridge: %s: %s", e.Kind, e.Message)
}

// IsAuthExpired 判断是否授权过期（前端触发重新授权）。
func IsAuthExpired(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Kind == ErrAuthExpired
}

// classifyError 按旧系统错误语义分类。
func classifyError(status int, msg string) *Error {
	switch {
	case containsAny(msg, "FEISHU_AUTH_EXPIRED", "auth", "token", "授权"):
		return &Error{Kind: ErrAuthExpired, Message: msg}
	case containsAny(msg, "ORDER_DATA_INVALID", "invalid", "字段", "校验"):
		return &Error{Kind: ErrOrderDataInvalid, Message: msg}
	case containsAny(msg, "SERVICE_MISCONFIGURED", "misconfig", "配置"):
		return &Error{Kind: ErrServiceMisconfigured, Message: msg}
	case containsAny(msg, "idempotency", "幂等", "duplicate"):
		return &Error{Kind: ErrIdempotencyConflict, Message: msg}
	default:
		return &Error{Kind: ErrUnknown, Message: msg}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && (len(s) >= len(sub)) && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
