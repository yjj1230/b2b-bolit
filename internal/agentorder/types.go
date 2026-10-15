package agentorder

import "encoding/json"

type DecisionStatus string

const (
	StatusValid             DecisionStatus = "VALID"
	StatusNeedsConfirmation DecisionStatus = "NEEDS_CONFIRMATION"
	StatusRejected          DecisionStatus = "REJECTED"
	StatusManualRequired    DecisionStatus = "MANUAL_REQUIRED"
)

type Candidate struct {
	CustomerID     string  `json:"customer_id"`
	GroupID        string  `json:"group_id"`
	SenderID       string  `json:"sender_id"`
	ProductID      string  `json:"product_id"`
	SKU            string  `json:"sku"`
	Quantity       int64   `json:"quantity"`
	Unit           string  `json:"unit"`
	UnitPriceCents int64   `json:"unit_price_cents"`
	PaymentTerm    string  `json:"payment_term"`
	ContractID     string  `json:"contract_id"`
	WarehouseID    string  `json:"warehouse_id"`
	ReceiveAddress string  `json:"receive_address"`
	Confidence     float64 `json:"confidence"`
}

type PolicyInput struct {
	TenantID      string          `json:"tenant_id"`
	CorrelationID string          `json:"correlation_id"`
	CustomerOwner string          `json:"customer_owner"`
	CustomerGrade string          `json:"customer_grade"`
	SenderAllowed bool            `json:"sender_allowed"`
	SKUMatches    int             `json:"sku_matches"`
	UnitRatio     int64           `json:"unit_ratio"`
	ContractPrice int64           `json:"contract_price_cents"`
	CreditLimit   int64           `json:"credit_limit_cents"`
	Outstanding   int64           `json:"outstanding_cents"`
	AmountLimit   int64           `json:"amount_limit_cents"`
	IsNewCustomer bool            `json:"is_new_customer"`
	Candidate     Candidate       `json:"candidate"`
	RawSnapshot   json.RawMessage `json:"raw_snapshot"`
}

type RuleHit struct {
	RuleID  string `json:"rule_id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

type PolicyDecision struct {
	Status        DecisionStatus  `json:"status"`
	PolicyVersion string          `json:"policy_version"`
	RuleHits      []RuleHit       `json:"rule_hits"`
	InputSnapshot json.RawMessage `json:"input_snapshot"`
	Output        map[string]any  `json:"output"`
}
