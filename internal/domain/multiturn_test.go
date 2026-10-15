package domain

import "testing"

func TestPreOrderMultiTurnStates(t *testing.T) {
	if !PreOrderFSM.Can(StatePreCaptured, StatePreCollecting) {
		t.Fatal("PRE_CAPTURED must enter PRE_COLLECTING")
	}
	if !PreOrderFSM.Can(StatePreCollecting, StatePreCollecting) {
		t.Fatal("PRE_COLLECTING must absorb another message")
	}
	if !PreOrderFSM.Can(StatePreCollecting, StatePrePendingConfirm) {
		t.Fatal("PRE_COLLECTING must move to confirmation")
	}
	if !PreOrderFSM.Can(StatePreCollecting, StatePreAmbiguous) {
		t.Fatal("PRE_COLLECTING must expose ambiguity")
	}
	if !IsTerminal(StatePreExpired) {
		t.Fatal("PRE_EXPIRED must be terminal")
	}
}
