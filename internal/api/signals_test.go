package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/workflows"
)

// AUTH-01 收敛：所有可承载 Operator 的信号类型都被覆盖为鉴权身份（覆盖 stampSignalOperator 全分支）。
func TestStampSignalOperatorAllTypes(t *testing.T) {
	payloads := []any{
		workflows.PriceReviewSignal{Operator: "forged"},
		&workflows.PriceReviewSignal{Operator: "forged"},
		workflows.ChairmanReviewSignal{Operator: "forged"},
		&workflows.ChairmanReviewSignal{Operator: "forged"},
		workflows.StockReviewSignal{Operator: "forged"},
		&workflows.StockReviewSignal{Operator: "forged"},
		workflows.WarehouseConfirmSignal{Operator: "forged"},
		&workflows.WarehouseConfirmSignal{Operator: "forged"},
		workflows.TransferDecisionSignal{Operator: "forged"},
		&workflows.TransferDecisionSignal{Operator: "forged"},
		workflows.PaymentConfirmSignal{Operator: "forged"},
		&workflows.PaymentConfirmSignal{Operator: "forged"},
		workflows.ShipActionSignal{Operator: "forged"},
		&workflows.ShipActionSignal{Operator: "forged"},
		workflows.ReceiptExReviewSignal{Operator: "forged"},
		&workflows.ReceiptExReviewSignal{Operator: "forged"},
		workflows.InvoiceIssuedSignal{Operator: "forged"},
		&workflows.InvoiceIssuedSignal{Operator: "forged"},
		workflows.PaymentTrackedSignal{Operator: "forged"},
		&workflows.PaymentTrackedSignal{Operator: "forged"},
		workflows.ModifyOrderSignal{Operator: "forged"},
		&workflows.ModifyOrderSignal{Operator: "forged"},
		workflows.ResubmitSignal{Operator: "forged"},
		&workflows.ResubmitSignal{Operator: "forged"},
		workflows.PreOrderConfirmSignal{Operator: "forged"},
		&workflows.PreOrderConfirmSignal{Operator: "forged"},
	}
	for _, p := range payloads {
		got := stampSignalOperator(p, "u-op")
		switch v := got.(type) {
		case workflows.PriceReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.PriceReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.ChairmanReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.ChairmanReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.StockReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.StockReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.WarehouseConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.WarehouseConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.TransferDecisionSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.TransferDecisionSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.PaymentConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.PaymentConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.ShipActionSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.ShipActionSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.ReceiptExReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.ReceiptExReviewSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.InvoiceIssuedSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.InvoiceIssuedSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.PaymentTrackedSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.PaymentTrackedSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.ModifyOrderSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.ModifyOrderSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.ResubmitSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.ResubmitSignal:
			require.Equal(t, "u-op", v.Operator)
		case workflows.PreOrderConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		case *workflows.PreOrderConfirmSignal:
			require.Equal(t, "u-op", v.Operator)
		default:
			t.Fatalf("unexpected payload type %T", got)
		}
	}
	// 不承载 Operator 的类型原样返回
	require.Equal(t, 42, stampSignalOperator(42, "u-op"))
	var rp workflows.ReceiptProvidedSignal
	require.Equal(t, rp, stampSignalOperator(rp, "u-op"))
}
