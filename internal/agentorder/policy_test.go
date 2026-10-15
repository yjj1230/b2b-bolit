package agentorder

import "testing"

func validPolicyInput() PolicyInput {
	return PolicyInput{
		TenantID: "t1", CustomerOwner: "u-sales", SenderAllowed: true, SKUMatches: 1, UnitRatio: 1,
		ContractPrice: 1000, CreditLimit: 100000, Candidate: Candidate{
			CustomerID: "c1", SenderID: "u-sales", ProductID: "p1", Quantity: 10,
			UnitPriceCents: 1000, PaymentTerm: "非款到", ContractID: "ct1", WarehouseID: "wh1", ReceiveAddress: "addr",
		},
	}
}

func TestEvaluatePolicyValid(t *testing.T) {
	d := EvaluatePolicy(validPolicyInput())
	if d.Status != StatusValid {
		t.Fatalf("status=%s, want %s", d.Status, StatusValid)
	}
	if len(d.RuleHits) == 0 || d.PolicyVersion == "" {
		t.Fatalf("missing deterministic audit result: %+v", d)
	}
}

func TestEvaluatePolicyRejectsUnauthorizedSender(t *testing.T) {
	in := validPolicyInput()
	in.SenderAllowed = false
	d := EvaluatePolicy(in)
	if d.Status != StatusRejected {
		t.Fatalf("status=%s, want %s", d.Status, StatusRejected)
	}
}

func TestEvaluatePolicyRequiresManualForAmbiguousSKU(t *testing.T) {
	in := validPolicyInput()
	in.SKUMatches = 2
	d := EvaluatePolicy(in)
	if d.Status != StatusManualRequired {
		t.Fatalf("status=%s, want %s", d.Status, StatusManualRequired)
	}
}
