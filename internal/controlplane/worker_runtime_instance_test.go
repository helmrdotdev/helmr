package controlplane

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestValidateRuntimeCleanupProofIsTypedAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
		workerapi.RuntimeCleanupNotMaterialized,
	} {
		if err := validateRuntimeCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	for _, proof := range []workerapi.RuntimeCleanupProof{
		{Method: "assumed", CompletedAt: now},
		{Method: workerapi.RuntimeCleanupHostReconciled},
		{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: now.Add(2 * time.Minute)},
	} {
		if err := validateRuntimeCleanupProof(proof, now); err == nil {
			t.Fatalf("invalid proof accepted: %+v", proof)
		}
	}
}

func TestValidateRuntimeClosedCleanupProofRequiresPhysicalTeardown(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
	} {
		if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{
		Method: workerapi.RuntimeCleanupNotMaterialized, CompletedAt: now,
	}, now); err == nil {
		t.Fatal("not_materialized proof released a closed runtime")
	}
}
