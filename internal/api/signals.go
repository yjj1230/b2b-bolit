package api

import (
	"encoding/json"
	"io"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/workflows"
)

// ── Signal 载荷解析（HTTP 请求体 → 工作流 Signal 结构）──
// 人工闸门不可绕过：Signal 只能通过这些受控载荷投递。

// orderSignalDecoders 订单信号解码表（signal 名 → 解码函数）。
var orderSignalDecoders = map[string]func(body io.Reader) (any, error){
	workflows.SignalPriceReview:      decodeJSON[workflows.PriceReviewSignal],
	workflows.SignalChairmanReview:   decodeJSON[workflows.ChairmanReviewSignal],
	workflows.SignalStockReview:      decodeJSON[workflows.StockReviewSignal],
	workflows.SignalWarehouseConfirm: decodeJSON[workflows.WarehouseConfirmSignal],
	workflows.SignalTransferDecision: decodeJSON[workflows.TransferDecisionSignal],
	workflows.SignalPaymentConfirm:   decodeJSON[workflows.PaymentConfirmSignal],
	workflows.SignalShipAction:       decodeJSON[workflows.ShipActionSignal],
	workflows.SignalReceiptExReview:  decodeJSON[workflows.ReceiptExReviewSignal],
	workflows.SignalReceiptProvided:  decodeJSON[workflows.ReceiptProvidedSignal],
	workflows.SignalInvoiceIssued:    decodeJSON[workflows.InvoiceIssuedSignal],
	workflows.SignalPaymentTracked:   decodeJSON[workflows.PaymentTrackedSignal],
	workflows.SignalModifyOrder:      decodeJSON[workflows.ModifyOrderSignal],
	workflows.SignalResubmit:         decodeJSON[workflows.ResubmitSignal],
}

// preOrderSignalDecoders 预订单信号解码表。
var preOrderSignalDecoders = map[string]func(body io.Reader) (any, error){
	workflows.SignalPreOrderConfirm: decodeJSON[workflows.PreOrderConfirmSignal],
	workflows.SignalPreOrderIgnore:  decodeJSON[workflows.PreOrderIgnoreSignal],
}

func decodeJSON[T any](body io.Reader) (any, error) {
	var v T
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// signalPayload 订单信号解析；body 为空时用空结构（如 ReceiptProvided）。
func signalPayload(signal string, body io.Reader) (any, bool) {
	dec, ok := orderSignalDecoders[signal]
	if !ok {
		return nil, false
	}
	v, err := dec(body)
	if err != nil {
		return nil, false
	}
	return v, true
}

// preOrderSignalPayload 预订单信号解析。
func preOrderSignalPayload(signal string, body io.Reader) (any, bool) {
	dec, ok := preOrderSignalDecoders[signal]
	if !ok {
		return nil, false
	}
	v, err := dec(body)
	if err != nil {
		return nil, false
	}
	return v, true
}

// stampSignalOperator 事件链可信（AUTH-01）：Operator 一律覆盖为鉴权后的当前用户，
// 禁止请求体伪造审批人（事件链/审批历史的 operator 必须真实）。
// 兼容 decodeJSON 的值语义与未来指针语义：返回修正后的载荷。
func stampSignalOperator(payload any, userID string) any {
	switch v := payload.(type) {
	case workflows.PriceReviewSignal:
		v.Operator = userID
		return v
	case *workflows.PriceReviewSignal:
		v.Operator = userID
		return v
	case workflows.ChairmanReviewSignal:
		v.Operator = userID
		return v
	case *workflows.ChairmanReviewSignal:
		v.Operator = userID
		return v
	case workflows.StockReviewSignal:
		v.Operator = userID
		return v
	case *workflows.StockReviewSignal:
		v.Operator = userID
		return v
	case workflows.WarehouseConfirmSignal:
		v.Operator = userID
		return v
	case *workflows.WarehouseConfirmSignal:
		v.Operator = userID
		return v
	case workflows.TransferDecisionSignal:
		v.Operator = userID
		return v
	case *workflows.TransferDecisionSignal:
		v.Operator = userID
		return v
	case workflows.PaymentConfirmSignal:
		v.Operator = userID
		return v
	case *workflows.PaymentConfirmSignal:
		v.Operator = userID
		return v
	case workflows.ShipActionSignal:
		v.Operator = userID
		return v
	case *workflows.ShipActionSignal:
		v.Operator = userID
		return v
	case workflows.ReceiptExReviewSignal:
		v.Operator = userID
		return v
	case *workflows.ReceiptExReviewSignal:
		v.Operator = userID
		return v
	case workflows.InvoiceIssuedSignal:
		v.Operator = userID
		return v
	case *workflows.InvoiceIssuedSignal:
		v.Operator = userID
		return v
	case workflows.PaymentTrackedSignal:
		v.Operator = userID
		return v
	case *workflows.PaymentTrackedSignal:
		v.Operator = userID
		return v
	case workflows.ModifyOrderSignal:
		v.Operator = userID
		return v
	case *workflows.ModifyOrderSignal:
		v.Operator = userID
		return v
	case workflows.ResubmitSignal:
		v.Operator = userID
		return v
	case *workflows.ResubmitSignal:
		v.Operator = userID
		return v
	case workflows.PreOrderConfirmSignal:
		v.Operator = userID
		return v
	case *workflows.PreOrderConfirmSignal:
		v.Operator = userID
		return v
	default:
		return payload
	}
}

// keyFor 预订单幂等键构造（透传 domain）。
func keyFor(kind, tenantID, groupID, msgID, intentVersion string) (string, error) {
	return domain.PreOrderKey(tenantID, groupID, msgID, intentVersion)
}

// sumQty 明细数量合计（投影金额）。
func sumQty(items []repo.OrderItem) int64 {
	var n int64
	for _, it := range items {
		n += it.Quantity
	}
	return n
}
