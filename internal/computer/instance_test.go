package computer

import (
	"errors"
	"testing"
	"time"
)

func TestCleanupProofIsTypedAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{CleanupMachineClosed, CleanupHostReconciled, CleanupNotMaterialized} {
		if _, err := (CleanupProof{Method: method, CompletedAt: now}).evidence(now, false); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	for _, proof := range []CleanupProof{
		{Method: "assumed", CompletedAt: now},
		{Method: CleanupHostReconciled},
		{Method: CleanupHostReconciled, CompletedAt: now.Add(2 * time.Minute)},
	} {
		var input InputError
		if _, err := proof.evidence(now, false); !errors.As(err, &input) {
			t.Fatalf("invalid proof accepted: %+v %v", proof, err)
		}
	}
}

func TestClosedCleanupProofRequiresPhysicalTeardown(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{CleanupMachineClosed, CleanupHostReconciled} {
		if _, err := (CleanupProof{Method: method, CompletedAt: now}).evidence(now, true); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	if _, err := (CleanupProof{Method: CleanupNotMaterialized, CompletedAt: now}).evidence(now, true); err == nil {
		t.Fatal("not_materialized proof released a closed instance")
	}
}
