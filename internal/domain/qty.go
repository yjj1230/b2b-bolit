package domain

import "fmt"

// ── 数量值对象（L0-06，docs/02-architecture.html：零/负/溢出拒绝）──

// ErrInvalidQuantity 数量非法（零/负）。
type ErrInvalidQuantity struct{ V int64 }

func (e *ErrInvalidQuantity) Error() string {
	return fmt.Sprintf("invalid quantity %d (must be positive)", e.V)
}

// ErrOverflow 算术溢出。
type ErrOverflow struct{ Op string }

func (e *ErrOverflow) Error() string { return "quantity overflow: " + e.Op }

// Qty 正数量值对象（订单明细/备货/发货/开票数量，> 0）。
type Qty int64

// NewQty 构造正数量（> 0）。
func NewQty(v int64) (Qty, error) {
	if v <= 0 {
		return 0, &ErrInvalidQuantity{V: v}
	}
	return Qty(v), nil
}

// Int64 返回原始值。
func (q Qty) Int64() int64 { return int64(q) }

// Add 加法（溢出拒绝）。
func (q Qty) Add(o Qty) (Qty, error) {
	s := q.Int64() + o.Int64()
	if (o.Int64() > 0 && s < q.Int64()) || (o.Int64() < 0 && s > q.Int64()) {
		return 0, &ErrOverflow{Op: "add"}
	}
	return Qty(s), nil
}

// Sub 减法（结果必须 > 0，否则 ErrInvalidQuantity）。
func (q Qty) Sub(o Qty) (Qty, error) {
	s := q.Int64() - o.Int64()
	if s <= 0 {
		return 0, &ErrInvalidQuantity{V: s}
	}
	return Qty(s), nil
}

// ── 非负数量（库存量，允许 0，禁止负数）──

// NonNegQty 非负数量值对象（库存 qty >= 0）。
type NonNegQty int64

// NewNonNegQty 构造非负数量（>= 0）。
func NewNonNegQty(v int64) (NonNegQty, error) {
	if v < 0 {
		return 0, &ErrInvalidQuantity{V: v}
	}
	return NonNegQty(v), nil
}

// Int64 返回原始值。
func (n NonNegQty) Int64() int64 { return int64(n) }

// Sub 减法（结果必须 >= 0）。
func (n NonNegQty) Sub(o NonNegQty) (NonNegQty, error) {
	s := n.Int64() - o.Int64()
	if s < 0 {
		return 0, &ErrInvalidQuantity{V: s}
	}
	return NonNegQty(s), nil
}
