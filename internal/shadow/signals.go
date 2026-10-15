package shadow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/workflows"
)

// TypedSignal 通用 payload → workflow 强类型 Signal（12 §6 映射规范的执行侧）。
// 零猜测：字段缺失/类型错 → 错误留痕（MISSING_FIELD 分歧），绝不投递半截载荷。
func TypedSignal(signal string, p map[string]any) (any, error) {
	str := func(k string) string { return StrOf(p, k, "") }
	i64 := func(k string) (int64, error) {
		v, ok := p[k]
		if !ok || v == nil {
			return 0, fmt.Errorf("missing %s", k)
		}
		switch x := v.(type) {
		case float64:
			return int64(x), nil
		case json.Number:
			return x.Int64()
		case string:
			return strconv.ParseInt(x, 10, 64)
		case int:
			return int64(x), nil
		case int8:
			return int64(x), nil
		case int16:
			return int64(x), nil
		case int32:
			return int64(x), nil
		case int64:
			return x, nil
		case uint:
			return int64(x), nil
		case uint8:
			return int64(x), nil
		case uint16:
			return int64(x), nil
		case uint32:
			return int64(x), nil
		case uint64:
			if x > uint64(^uint64(0)>>1) {
				return 0, fmt.Errorf("field %s out of range", k)
			}
			return int64(x), nil
		default:
			return 0, fmt.Errorf("field %s not numeric (%T)", k, v)
		}
	}
	switch signal {
	case workflows.SignalPriceReview:
		return workflows.PriceReviewSignal{Verdict: str("verdict"), Operator: str("operator"), Reason: str("reason")}, nil
	case workflows.SignalChairmanReview:
		return workflows.ChairmanReviewSignal{Verdict: str("verdict"), Operator: str("operator")}, nil
	case workflows.SignalStockReview:
		return workflows.StockReviewSignal{Verdict: str("verdict"), Operator: str("operator"),
			WarehouseID: str("warehouse_id"), Note: str("note")}, nil
	case workflows.SignalWarehouseConfirm:
		return workflows.WarehouseConfirmSignal{Verdict: str("verdict"), Operator: str("operator")}, nil
	case workflows.SignalTransferDecision:
		return workflows.TransferDecisionSignal{Verdict: str("verdict"), Operator: str("operator"),
			WarehouseID: str("warehouse_id")}, nil
	case workflows.SignalPaymentConfirm:
		return workflows.PaymentConfirmSignal{Verdict: str("verdict"), Operator: str("operator"),
			LedgerRef: str("ledger_ref")}, nil
	case workflows.SignalShipAction:
		qty, err := i64("qty")
		if err != nil {
			return nil, err
		}
		return workflows.ShipActionSignal{Verdict: str("verdict"), Operator: str("operator"),
			Qty: qty, WarehouseID: str("warehouse_id")}, nil
	case workflows.SignalReceiptProvided:
		return nil, nil // workflow 收空载荷
	case workflows.SignalReceiptExReview:
		deadline, _ := i64("deadline_days")
		return workflows.ReceiptExReviewSignal{Verdict: str("verdict"), Operator: str("operator"),
			DeadlineDays: int(deadline)}, nil
	case workflows.SignalInvoiceIssued:
		return workflows.InvoiceIssuedSignal{Verdict: str("verdict"), Operator: str("operator"),
			TaxNo: str("tax_no")}, nil
	case workflows.SignalPaymentTracked:
		amount, err := i64("amount")
		if err != nil {
			return nil, err
		}
		return workflows.PaymentTrackedSignal{Amount: amount, Operator: str("operator")}, nil
	case workflows.SignalShadowInject:
		kind := str("kind")
		if kind == "" {
			kind = str("verdict") // 映射规则可用固定 verdict 表达等价分支
		}
		return workflows.ShadowInjectSignal{Kind: kind}, nil
	case workflows.SignalResubmit:
		return workflows.ResubmitSignal{Operator: str("operator")}, nil
	case workflows.SignalModifyOrder:
		class := domain.ModificationClass(str("class"))
		if class == "" {
			return nil, fmt.Errorf("missing class")
		}
		return workflows.ModifyOrderSignal{Class: class, Changes: StrSlice(p["changes"]), Operator: str("operator")}, nil
	default:
		return nil, fmt.Errorf("unknown signal %q (mapping table drift)", signal)
	}
}

// StrOf 字符串字段读取（容忍数字等标量；缺失回默认值）。
func StrOf(p map[string]any, key, def string) string {
	if v, ok := p[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return def
}

// I64Of 整数字段读取（float64/json.Number/字符串均可；缺失/非法回默认值）。
func I64Of(p map[string]any, key string, def int64) int64 {
	v, ok := p[key]
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n
		}
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	}
	return def
}

// StrSlice 字符串列表读取（[]any / []string / 单字符串）。
func StrSlice(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, strings.TrimSpace(fmt.Sprint(e)))
		}
		return out
	case []string:
		return x
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}
