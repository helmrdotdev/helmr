package controlplane

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validRunLeaseAssignment(workerID uuid.UUID) workerapi.RunLeaseAssignment {
	return workerapi.RunLeaseAssignment{
		ID: uuid.NewV7().String(), RunID: uuid.NewV7().String(),
		AttemptNumber: 1, LeaseSequence: 1, WorkerGroupID: "worker-group",
		WorkerInstanceID: workerID.String(), WorkerEpoch: 1,
		RuntimeInstanceID: uuid.NewV7().String(), RuntimeIdentityID: "runtime-identity",
		WorkspaceID:            uuid.NewV7().String(),
		WorkspaceMountID:       uuid.NewV7().String(),
		WorkspaceLeaseID:       uuid.NewV7().String(),
		BaseWorkspaceVersionID: uuid.NewV7().String(),
		OwnershipGeneration:    1, WriterGeneration: 1, MountFencingGeneration: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1024,
		RequestedGuestEphemeralDiskBytes: 1024, RequestedExecutionSlots: 1,
		MaxActiveDurationMs: 60_000, StartDeadlineAt: time.Now().Add(time.Minute).UTC(),
		ExpiresAt: time.Now().Add(2 * time.Minute).UTC(),
	}
}

func TestParseRunLeaseFence(t *testing.T) {
	fence := workerapi.RunLeaseFence{
		ID: uuid.NewV7().String(), LeaseSequence: 3,
	}
	parsed, err := parseRunLeaseFence(fence)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.leaseID.String() != fence.ID {
		t.Fatalf("parsed fence = %+v", parsed)
	}
}

func TestParseRunLeaseFenceRejectsInvalidIdentity(t *testing.T) {
	for _, fence := range []workerapi.RunLeaseFence{
		{ID: "not-a-uuid", LeaseSequence: 1},
		{ID: uuid.NewV7().String(), LeaseSequence: 0},
	} {
		if _, err := parseRunLeaseFence(fence); err == nil {
			t.Fatalf("accepted invalid fence %+v", fence)
		}
	}
}
