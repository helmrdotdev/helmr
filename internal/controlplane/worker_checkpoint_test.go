package controlplane

import "testing"

func TestTokenWaitDecisionPreservesExpiredTokenFailure(t *testing.T) {
	kind, payload, err := tokenWaitDecision("failed", nil, "token_expired")
	if err != nil || kind != "failed" || string(payload) != `{"reason_code":"token_expired"}` {
		t.Fatalf("decision = %q %s, err=%v", kind, payload, err)
	}
}

func TestTokenWaitDecisionIncludesCancellationReason(t *testing.T) {
	kind, payload, err := tokenWaitDecision("cancelled", nil, "token_cancelled")
	if err != nil || kind != "cancelled" || string(payload) != `{"reason_code":"token_cancelled"}` {
		t.Fatalf("decision = %q %s, err=%v", kind, payload, err)
	}
}
