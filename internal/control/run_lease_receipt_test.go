package control

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

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
