package domain

import "fmt"

// ── 金额值对象（分，>= 0；L0-06）──

// ErrInvalidAmount 金额非法（负数）。
type ErrInvalidAmount struct{ V int64 }

func (e *ErrInvalidAmount) Error() string {
	return fmt.Sprintf("invalid amount %d cents (must be non-negative)", e.V)
}

// AmountCents 金额值对象（单位：分，>= 0）。
type AmountCents int64

// NewAmountCents 构造金额（>= 0）。
func NewAmountCents(v int64) (AmountCents, error) {
	if v < 0 {
		return 0, &ErrInvalidAmount{V: v}
	}
	return AmountCents(v), nil
}

// Int64 返回原始值。
func (a AmountCents) Int64() int64 { return int64(a) }

// Add 加法（溢出拒绝）。
func (a AmountCents) Add(o AmountCents) (AmountCents, error) {
	s := a.Int64() + o.Int64()
	if s < a.Int64() {
		return 0, &ErrOverflow{Op: "amount add"}
	}
	return AmountCents(s), nil
}

// Sub 减法（结果必须 >= 0）。
func (a AmountCents) Sub(o AmountCents) (AmountCents, error) {
	s := a.Int64() - o.Int64()
	if s < 0 {
		return 0, &ErrInvalidAmount{V: s}
	}
	return AmountCents(s), nil
}

// MulQty 金额 × 数量（开票金额计算，溢出拒绝）。
func (a AmountCents) MulQty(q Qty) (AmountCents, error) {
	v := a.Int64() * q.Int64()
	if q.Int64() != 0 && v/q.Int64() != a.Int64() {
		return 0, &ErrOverflow{Op: "amount mul"}
	}
	return AmountCents(v), nil
}
