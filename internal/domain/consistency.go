package domain

import "fmt"

// ── 守恒断言纯函数（L0-07，docs/01-core-graph.html §1.2 constraints）──
// 数量守恒基准：order_items.quantity（唯一事实源）；所有断言为纯函数，
// 持久层在事务内以同样规则校验（防线 5）。

// ErrConservation 守恒被破坏。
type ErrConservation struct {
	Kind string // 违反复核类型
	Msg  string
}

func (e *ErrConservation) Error() string {
	return fmt.Sprintf("%s conservation violated: %s", e.Kind, e.Msg)
}

// IsConservation 判断错误是否为守恒违例。
func IsConservation(err error) bool {
	_, ok := err.(*ErrConservation)
	return ok
}

// AssertPrepareWithinOrder 备货守恒（CQ-06）：∑(备货确认量) ≤ 订单数量（每明细）。
// prepared 为按 order_item 累计的备货确认量；orderQty 为订单数量。
func AssertPrepareWithinOrder(prepared, orderQty int64) error {
	if prepared < 0 || orderQty < 0 {
		return &ErrConservation{Kind: "prepare", Msg: fmt.Sprintf("negative input prepared=%d orderQty=%d", prepared, orderQty)}
	}
	if prepared > orderQty {
		return &ErrConservation{Kind: "prepare", Msg: fmt.Sprintf("prepared %d > order %d", prepared, orderQty)}
	}
	return nil
}

// AssertShipWithinPrepare 发货守恒（CQ-07）：∑(发货量) ≤ ∑(备货量)。
func AssertShipWithinPrepare(shipped, prepared int64) error {
	if shipped < 0 || prepared < 0 {
		return &ErrConservation{Kind: "ship", Msg: fmt.Sprintf("negative input shipped=%d prepared=%d", shipped, prepared)}
	}
	if shipped > prepared {
		return &ErrConservation{Kind: "ship", Msg: fmt.Sprintf("shipped %d > prepared %d", shipped, prepared)}
	}
	return nil
}

// ShipCompletion 判断订单全部发货完成条件（CQ-07）：累计发货 = 订单数量。
func ShipCompletion(shippedTotal, orderQty int64) bool {
	return shippedTotal == orderQty
}

// AssertInvoiceWithinShipped 开票守恒（CQ-09，金额+数量双维度）：
// ∑(开票金额) ≤ ∑(可开票金额)；∑(开票数量) ≤ ∑(有效发货数量)。
// 任一维度超限均拒绝（开票不超发货，回单不卡开票）。
func AssertInvoiceWithinShipped(invQty, invAmount, shippedQty, shippedAmount int64) error {
	if invQty < 0 || invAmount < 0 || shippedQty < 0 || shippedAmount < 0 {
		return &ErrConservation{Kind: "invoice", Msg: "negative input"}
	}
	if invQty > shippedQty {
		return &ErrConservation{Kind: "invoice", Msg: fmt.Sprintf("invoice qty %d > shipped qty %d", invQty, shippedQty)}
	}
	if invAmount > shippedAmount {
		return &ErrConservation{Kind: "invoice", Msg: fmt.Sprintf("invoice amount %d > shipped amount %d", invAmount, shippedAmount)}
	}
	return nil
}

// AssertInventoryNonNeg 库存守恒（CQ-08）：库存恒 >= 0（出与发货同事务）。
func AssertInventoryNonNeg(current, outgoing int64) error {
	if current < 0 {
		return &ErrConservation{Kind: "inventory", Msg: fmt.Sprintf("inventory %d < 0", current)}
	}
	if outgoing > current {
		return &ErrConservation{Kind: "inventory", Msg: fmt.Sprintf("outgoing %d > inventory %d", outgoing, current)}
	}
	return nil
}
