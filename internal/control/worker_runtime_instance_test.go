package control

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestValidateRuntimeCleanupProofIsTypedAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		api.WorkerRuntimeCleanupSessionClosed,
		api.WorkerRuntimeCleanupHostReconciled,
		api.WorkerRuntimeCleanupNotMaterialized,
	} {
		if err := validateRuntimeCleanupProof(api.WorkerRuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	for _, proof := range []api.WorkerRuntimeCleanupProof{
		{Method: "assumed", CompletedAt: now},
		{Method: api.WorkerRuntimeCleanupHostReconciled},
		{Method: api.WorkerRuntimeCleanupHostReconciled, CompletedAt: now.Add(2 * time.Minute)},
	} {
		if err := validateRuntimeCleanupProof(proof, now); err == nil {
			t.Fatalf("invalid proof accepted: %+v", proof)
		}
	}
}

func TestValidateRuntimeClosedCleanupProofRequiresPhysicalTeardown(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		api.WorkerRuntimeCleanupSessionClosed,
		api.WorkerRuntimeCleanupHostReconciled,
	} {
		if err := validateRuntimeClosedCleanupProof(api.WorkerRuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	if err := validateRuntimeClosedCleanupProof(api.WorkerRuntimeCleanupProof{
		Method: api.WorkerRuntimeCleanupNotMaterialized, CompletedAt: now,
	}, now); err == nil {
		t.Fatal("not_materialized proof released a closed runtime")
	}
}
