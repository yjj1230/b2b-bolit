package agentorder

import (
	"encoding/json"
	"fmt"
)

const DefaultPolicyVersion = "agent-order-policy-v1"

// EvaluatePolicy 只依赖输入快照，保证同一输入在重放时得到相同结论。
func EvaluatePolicy(in PolicyInput) PolicyDecision {
	res := PolicyDecision{Status: StatusValid, PolicyVersion: DefaultPolicyVersion, Output: map[string]any{}}
	hit := func(id, outcome, reason string) {
		res.RuleHits = append(res.RuleHits, RuleHit{RuleID: id, Outcome: outcome, Reason: reason})
	}
	require := func(ok bool, id, reason string, status DecisionStatus) {
		if !ok && statusRank(status) > statusRank(res.Status) {
			res.Status = status
		}
		hit(id, map[bool]string{true: "PASS", false: "FAIL"}[ok], reason)
	}

	require(in.CustomerOwner != "" && in.CustomerOwner == in.Candidate.SenderID, "customer.sender_owner", "发送者不属于客户归属范围", StatusRejected)
	require(in.SenderAllowed, "customer.sender_permission", "发送者没有下单权限", StatusRejected)
	require(in.SKUMatches == 1 && in.Candidate.ProductID != "", "product.unique_match", "SKU 无法唯一匹配", StatusManualRequired)
	require(in.UnitRatio > 0, "product.unit_conversion", "单位换算缺失", StatusManualRequired)
	require(in.Candidate.Quantity > 0, "order.quantity_positive", "数量必须大于零", StatusRejected)
	require(in.Candidate.UnitPriceCents >= 0, "order.price_nonnegative", "价格不能为负数", StatusRejected)
	if in.ContractPrice > 0 && in.Candidate.UnitPriceCents != in.ContractPrice {
		require(false, "price.contract_price", "协议价不一致", StatusNeedsConfirmation)
	} else {
		require(true, "price.contract_price", "协议价匹配", StatusNeedsConfirmation)
	}
	require(in.Candidate.ContractID != "", "contract.required", "合同或协议缺失", StatusManualRequired)
	require(in.Candidate.PaymentTerm != "", "payment_term.required", "账期缺失", StatusManualRequired)
	require(in.CreditLimit <= 0 || in.Outstanding+in.Candidate.UnitPriceCents*in.Candidate.Quantity <= in.CreditLimit,
		"credit.limit", "超过客户信用额度", StatusNeedsConfirmation)
	if in.AmountLimit > 0 && in.Candidate.UnitPriceCents*in.Candidate.Quantity > in.AmountLimit {
		require(false, "risk.amount_threshold", "超过金额阈值，需要人工审批", StatusNeedsConfirmation)
	}
	if in.IsNewCustomer {
		require(false, "risk.new_customer", "新客户必须人工确认", StatusManualRequired)
	}
	require(in.Candidate.WarehouseID != "", "delivery.warehouse", "仓库候选缺失", StatusManualRequired)
	require(in.Candidate.ReceiveAddress != "", "delivery.address", "收货地址缺失", StatusManualRequired)

	if res.Status == StatusValid && len(res.RuleHits) > 0 {
		res.Output["auto_derive_allowed"] = true
	}
	if b, err := json.Marshal(in); err == nil {
		res.InputSnapshot = b
	}
	return res
}

func statusRank(status DecisionStatus) int {
	switch status {
	case StatusRejected:
		return 4
	case StatusManualRequired:
		return 3
	case StatusNeedsConfirmation:
		return 2
	case StatusValid:
		return 1
	default:
		return 0
	}
}

func (d PolicyDecision) Validate() error {
	if d.PolicyVersion == "" {
		return fmt.Errorf("policy version is required")
	}
	if d.Status == "" {
		return fmt.Errorf("policy status is required")
	}
	return nil
}
