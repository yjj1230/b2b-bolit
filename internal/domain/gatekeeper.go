package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ── 发群守门员（docs/v0.1/05-business-flow.html N8 / 09-testing N-01）──
// 任何内容发客户群前必须有销售点一次确认（第一轮全部强制，先收紧后放行）。
// 纯函数域：状态机 + 幂等键 + 内容校验，零依赖（L0 覆盖）。

const (
	// GatekeeperPending 待确认（销售已点「发给客户」，系统弹守门员确认）。
	GatekeeperPending = "GATEKEEPER_PENDING"
	// GatekeeperConfirmed 已确认（发送；确认只发生一次）。
	GatekeeperConfirmed = "GATEKEEPER_CONFIRMED"
	// GatekeeperCancelled 取消（保留在详情页，不发送）。
	GatekeeperCancelled = "GATEKEEPER_CANCELLED"
)

// GatekeeperFSM 守门员状态机：待确认 → 已确认 / 取消（终态）。
var GatekeeperFSM = NewFSM("GATEKEEPER", GatekeeperPending, map[string][]string{
	GatekeeperPending: {
		GatekeeperConfirmed,
		GatekeeperCancelled,
	},
})

// GatekeeperContentHash 发群内容指纹（幂等：确认只发生一次；
// 同一订单同一内容重复确认 → 命中既有确认记录，不重复发送/不重复记录事件）。
func GatekeeperContentHash(content string) string {
	// 基线：内容字符串规范化（去首尾空白 + 折叠连续空白）后取 sha256 前 32 位；
	// 与事件 detail 中的 content_hash 对齐（API 层写入）。
	h := sha256Sum(normalizeContent(content))
	return h[:32]
}

// GatekeeperKey 守门员幂等键（事件链定位：同一订单同一内容只有一条确认）。
func GatekeeperKey(tenantID, orderID, contentHash string) string {
	return "gk:" + tenantID + ":" + orderID + ":" + contentHash
}

// ValidateGatekeeperContent 发群内容校验：非空且长度受限（防御性上限 8KB）。
func ValidateGatekeeperContent(content string) error {
	c := normalizeContent(content)
	if c == "" {
		return fmt.Errorf("发群内容为空")
	}
	if len(c) > 8192 {
		return fmt.Errorf("发群内容超长: %d bytes > 8192", len(c))
	}
	return nil
}

// ConfirmGatekeeper 确认迁移：Pending → Confirmed 合法；重复确认返回幂等错误哨兵。
func ConfirmGatekeeper(from string) error {
	if from == GatekeeperConfirmed {
		return ErrGatekeeperAlreadyConfirmed
	}
	if err := GatekeeperFSM.Validate(from, GatekeeperConfirmed); err != nil {
		return err
	}
	return nil
}

// CancelGatekeeper 取消迁移：Pending → Cancelled。
func CancelGatekeeper(from string) error {
	return GatekeeperFSM.Validate(from, GatekeeperCancelled)
}

// ErrGatekeeperAlreadyConfirmed 重复确认哨兵（API 层映射为幂等回执 200/409 语义）。
var ErrGatekeeperAlreadyConfirmed = fmt.Errorf("gatekeeper already confirmed")

// normalizeContent 规范化：TrimSpace + 折叠连续空白。
func normalizeContent(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// sha256Sum sha256 hex 摘要（与 canonicalizer 无关；仅用于内容指纹）。
func sha256Sum(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

// ── 商务备货选仓（docs/v0.1/05 N4 / 09-testing N-03）──

// ValidateStockReviewVerdict 备货闸门裁决：APPROVED / RETURNED。
func ValidateStockReviewVerdict(v string) bool {
	return v == "APPROVED" || v == "RETURNED"
}

// ValidateWarehouseSelection 选仓校验：通过时必须给出仓库；驳回可省略。
func ValidateWarehouseSelection(verdict, warehouseID string) error {
	if verdict == "APPROVED" && strings.TrimSpace(warehouseID) == "" {
		return fmt.Errorf("备货通过必须选备货仓")
	}
	return nil
}

// InvoiceLinkKey 发票-订单关联幂等键（追加关联防重复）。
func InvoiceLinkKey(tenantID, invoiceID, orderID string) string {
	return "invlink:" + tenantID + ":" + invoiceID + ":" + orderID
}
