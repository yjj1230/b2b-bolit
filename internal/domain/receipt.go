package domain

// ── 回单四分流判定（L0-10，docs/01-core-graph.html §2.5 + docs/03-testing.html L0-10）──
//
// INV 段入口分流（回单状态 CQ-10 与开票状态解耦，仅作例外审核触发条件）：
//   分流① 款到发货直接进入开票申请
//   分流② 有回单 → 进入开票申请
//   分流③ 无回单超 5 天（Timer 自动进入）→ 例外审核
//   分流④ 5 天内提交人提前申请 → 例外审核

// ReceiptSplit 分流结果。
type ReceiptSplit string

const (
	SplitDirectInvoice    ReceiptSplit = "DIRECT_INVOICE"    // 分流①：款到发货直接进入
	SplitHaveReceipt      ReceiptSplit = "HAVE_RECEIPT"      // 分流②：有回单
	SplitReceiptException ReceiptSplit = "RECEIPT_EXCEPTION" // 分流③④：无回单超 5 天 / 提前申请 → 例外审核
)

// ReceiptInput 回单分流判定输入。
type ReceiptInput struct {
	Paid              bool // 款到发货（已确认到账）
	HasReceipt        bool // 有回单
	DaysSinceShipment int  // 距最近发货天数
	EarlyApply        bool // 5 天内提交人提前申请（业务放行例外入口）
}

// ClassifyReceipt 判定回单分流。判定优先级：提前申请/超 5 天 → 例外；款到 → 直接；有回单 → 直接。
func ClassifyReceipt(in ReceiptInput) ReceiptSplit {
	if in.EarlyApply || (!in.HasReceipt && in.DaysSinceShipment > 5) {
		return SplitReceiptException
	}
	if in.Paid || in.HasReceipt {
		return SplitDirectInvoice
	}
	return SplitReceiptException
}
