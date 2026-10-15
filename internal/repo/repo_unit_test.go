package repo

import (
	"fmt"
	"testing"

	"b2b_pilot_v3/internal/domain"
)

// 纯函数单元测试（无 PG 依赖；错误类型/工具函数/键派生）

func TestHashPayload(t *testing.T) {
	h1 := HashPayload([]byte(`{"a":1}`))
	h2 := HashPayload([]byte(`{"a":1}`))
	h3 := HashPayload([]byte(`{"a":2}`))
	if h1 != h2 {
		t.Fatal("same payload must hash identically")
	}
	if h1 == h3 {
		t.Fatal("different payload must hash differently")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex length = %d", len(h1))
	}
}

func TestErrorTypes(t *testing.T) {
	nf := &ErrNotFound{What: "x"}
	if !IsNotFound(nf) {
		t.Fatal("IsNotFound should match ErrNotFound")
	}
	if IsNotFound(nil) || IsNotFound(&domain.ErrIllegalTransition{FSM: "T", From: "A", To: "B"}) {
		t.Fatal("IsNotFound must not match other errors")
	}
	ic := &ErrIdempotencyConflict{Key: "k"}
	if !IsIdempotencyConflict(ic) {
		t.Fatal("IsIdempotencyConflict should match")
	}
	if IsIdempotencyConflict(nil) {
		t.Fatal("nil is not a conflict")
	}
	if nf.Error() == "" || ic.Error() == "" {
		t.Fatal("errors must have messages")
	}
	ae := &ErrAlreadyExists{Msg: "m"}
	if ae.Error() == "" {
		t.Fatal("ErrAlreadyExists must have message")
	}
}

func TestShortIDDeterministic(t *testing.T) {
	a := shortID("key-1")
	b := shortID("key-1")
	c := shortID("key-2")
	if a != b {
		t.Fatal("shortID must be deterministic")
	}
	if a == c {
		t.Fatal("different keys must differ")
	}
	if len(a) != 12 {
		t.Fatalf("shortID length = %d", len(a))
	}
}

func TestMarshalJSONDetail(t *testing.T) {
	b := MarshalJSONDetail(map[string]any{"x": 1})
	if string(b) != `{"x":1}` {
		t.Fatalf("marshal = %s", b)
	}
	// 不可序列化值 → 兜底 {}
	bad := MarshalJSONDetail(func() {})
	if string(bad) != "{}" {
		t.Fatalf("bad marshal fallback = %s", bad)
	}
}

// 错误注入：类型断言跨包（authz 复用 repo 错误判断）
func TestIsNotFoundWrapped(t *testing.T) {
	err := fmt.Errorf("wrap: %w", &ErrNotFound{What: "order X"})
	if !IsNotFound(err) {
		t.Fatal("wrapped ErrNotFound should still match (errors.As)")
	}
}

// repo 工具补充覆盖（Pool/New 错误路径/对账脚本辅助）
func TestRepoInfraEdges(t *testing.T) {
	// New 错误 DSN
	if _, err := New(t.Context(), "postgres://bad:dsn@127.0.0.1:1/x?sslmode=disable&connect_timeout=1"); err == nil {
		t.Fatal("bad DSN should error")
	}
}

// 连接池关闭后 Begin 报错（txwrappers 错误分支）
func TestBeginAfterClose(t *testing.T) {
	dsn := "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"
	r, err := New(t.Context(), dsn)
	if err != nil {
		t.Skipf("PG unavailable: %v", err)
	}
	r.Close()
	if _, err := r.Begin(t.Context()); err == nil {
		t.Fatal("Begin after Close should error")
	}
	// Close 幂等
	r.Close()
}
