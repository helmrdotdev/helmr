package control

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestParseRunLeaseReceipt(t *testing.T) {
	receipt := validRunLeaseReceipt(uuid.Must(uuid.NewV7()))
	if _, err := parseRunLeaseReceipt(receipt); err != nil {
		t.Fatal(err)
	}

	nonCanonical := receipt
	nonCanonical.ID = "{" + receipt.ID + "}"
	if _, err := parseRunLeaseReceipt(nonCanonical); err == nil {
		t.Fatal("non-canonical UUID was accepted")
	}

	nilID := receipt
	nilID.ID = uuid.Nil.String()
	if _, err := parseRunLeaseReceipt(nilID); err == nil {
		t.Fatal("nil UUID was accepted")
	}

	invalidFence := receipt
	invalidFence.WriterGeneration = 0
	if _, err := parseRunLeaseReceipt(invalidFence); err == nil {
		t.Fatal("zero writer generation was accepted")
	}

	invalidDeadline := receipt
	invalidDeadline.StartDeadlineAt = invalidDeadline.ExpiresAt.Add(time.Second)
	if _, err := parseRunLeaseReceipt(invalidDeadline); err == nil {
		t.Fatal("deadline after expiry was accepted")
	}
}

func validRunLeaseReceipt(workerID uuid.UUID) api.WorkerRunLeaseReceipt {
	return api.WorkerRunLeaseReceipt{
		ID:                         uuid.Must(uuid.NewV7()).String(),
		RunID:                      uuid.Must(uuid.NewV7()).String(),
		AttemptNumber:              1,
		LeaseSequence:              1,
		WorkerGroupID:              "worker-group",
		WorkerInstanceID:           workerID.String(),
		WorkerEpoch:                1,
		WorkerProtocolVersion:      api.CurrentWorkerProtocolVersion,
		RuntimeInstanceID:          uuid.Must(uuid.NewV7()).String(),
		RuntimeIdentityID:          "runtime-identity",
		NetworkSlotID:              uuid.Must(uuid.NewV7()).String(),
		NetworkSlotGeneration:      1,
		WorkspaceID:                uuid.Must(uuid.NewV7()).String(),
		WorkspaceMountID:           uuid.Must(uuid.NewV7()).String(),
		WorkspaceLeaseID:           uuid.Must(uuid.NewV7()).String(),
		BaseWorkspaceVersionID:     uuid.Must(uuid.NewV7()).String(),
		OwnershipGeneration:        1,
		WriterGeneration:           1,
		MountFencingGeneration:     1,
		RequestedCPUMillis:         1000,
		RequestedMemoryBytes:       1024,
		RequestedWorkloadDiskBytes: 1024,
		RequestedExecutionSlots:    1,
		MaxActiveDurationMs:        60_000,
		StartDeadlineAt:            time.Now().Add(time.Minute).UTC(),
		ExpiresAt:                  time.Now().Add(2 * time.Minute).UTC(),
	}
}

func TestEqualRunLeaseReceipt(t *testing.T) {
	base := api.WorkerRunLeaseReceipt{
		ID:                         "lease",
		RunID:                      "run",
		AttemptNumber:              1,
		LeaseSequence:              2,
		WorkerGroupID:              "group",
		WorkerInstanceID:           "worker",
		WorkerEpoch:                3,
		WorkerProtocolVersion:      "v0",
		RuntimeInstanceID:          "runtime",
		RuntimeIdentityID:          "runtime-identity",
		NetworkSlotID:              "slot",
		NetworkSlotGeneration:      4,
		WorkspaceID:                "workspace",
		WorkspaceMountID:           "mount",
		WorkspaceLeaseID:           "workspace-lease",
		BaseWorkspaceVersionID:     "base",
		OwnershipGeneration:        5,
		WriterGeneration:           6,
		MountFencingGeneration:     7,
		RequestedCPUMillis:         8,
		RequestedMemoryBytes:       9,
		RequestedWorkloadDiskBytes: 10,
		RequestedScratchBytes:      11,
		RequestedExecutionSlots:    12,
		MaxActiveDurationMs:        13,
		ActiveElapsedMs:            14,
		Trace: api.TraceContext{
			TraceID:     "trace",
			SpanID:      "span",
			Traceparent: "traceparent",
		},
		StartDeadlineAt: time.Unix(1_000, 123).In(time.FixedZone("receipt", 3600)),
		ExpiresAt:       time.Unix(2_000, 456).In(time.FixedZone("receipt", 3600)),
	}

	sameInstant := base
	sameInstant.StartDeadlineAt = base.StartDeadlineAt.UTC()
	sameInstant.ExpiresAt = base.ExpiresAt.UTC()
	if !equalRunLeaseReceipt(base, sameInstant) {
		t.Fatal("equal receipt instants were rejected")
	}

	changedFence := base
	changedFence.WriterGeneration++
	if equalRunLeaseReceipt(base, changedFence) {
		t.Fatal("changed receipt fence was accepted")
	}

	changedDeadline := base
	changedDeadline.StartDeadlineAt = changedDeadline.StartDeadlineAt.Add(time.Nanosecond)
	if equalRunLeaseReceipt(base, changedDeadline) {
		t.Fatal("changed start deadline was accepted")
	}

	changedExpiry := base
	changedExpiry.ExpiresAt = changedExpiry.ExpiresAt.Add(time.Nanosecond)
	if equalRunLeaseReceipt(base, changedExpiry) {
		t.Fatal("changed expiry was accepted")
	}
}

func TestEqualCurrentOrPreviousRunLeaseReceipt(t *testing.T) {
	current := validRunLeaseReceipt(uuid.Must(uuid.NewV7()))
	previousExpiry := current.ExpiresAt.Add(-30 * time.Second)
	previous := current
	previous.ExpiresAt = previousExpiry
	if !equalCurrentOrPreviousRunLeaseReceipt(current, previous, pgvalue.Timestamptz(previousExpiry)) {
		t.Fatal("previous renewal expiry was rejected")
	}
	stale := previous
	stale.ExpiresAt = stale.ExpiresAt.Add(-time.Nanosecond)
	if equalCurrentOrPreviousRunLeaseReceipt(current, stale, pgvalue.Timestamptz(previousExpiry)) {
		t.Fatal("older renewal expiry was accepted")
	}
	changedFence := previous
	changedFence.WriterGeneration++
	if equalCurrentOrPreviousRunLeaseReceipt(current, changedFence, pgvalue.Timestamptz(previousExpiry)) {
		t.Fatal("changed previous-generation fence was accepted")
	}
}
