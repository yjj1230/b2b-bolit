package main

import (
	"testing"

	"b2b_pilot_v3/internal/authz"
)

func TestParsePrincipals(t *testing.T) {
	got := parsePrincipals("u-op:OP, u-accounting:ACCOUNTING ,bogus")
	if got["u-op"] != authz.RoleOP || got["u-accounting"] != authz.RoleAccounting {
		t.Fatalf("principals: %+v", got)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}
