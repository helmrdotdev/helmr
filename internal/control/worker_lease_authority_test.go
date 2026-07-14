package control

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestParseWorkerRunLeaseUsesDurableTypedFences(t *testing.T) {
	lease := api.WorkerRunLease{
		ID: "00000000-0000-0000-0000-000000000001", OrgID: "00000000-0000-0000-0000-000000000002",
		RunID: "00000000-0000-0000-0000-000000000003", WorkerGroupID: "group-1",
		WorkerInstanceID: "00000000-0000-0000-0000-000000000004", WorkerEpoch: 5, LeaseSequence: 6, SnapshotVersion: 9,
		RuntimeInstanceID: "00000000-0000-0000-0000-000000000005",
		NetworkSlotID:     "00000000-0000-0000-0000-000000000006", NetworkSlotGeneration: 7,
		ProtocolVersion: "helmr.worker.v0", AttemptNumber: 8,
	}

	got, err := parseWorkerRunLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	if got.workerGroupID != lease.WorkerGroupID || got.workerEpoch != lease.WorkerEpoch ||
		got.leaseSequence != lease.LeaseSequence || got.networkSlotGeneration != lease.NetworkSlotGeneration ||
		got.attemptNumber != lease.AttemptNumber {
		t.Fatalf("parsed lease = %+v", got)
	}
}

func TestParseWorkerRunLeaseRejectsMissingPhysicalFence(t *testing.T) {
	lease := api.WorkerRunLease{
		ID: "00000000-0000-0000-0000-000000000001", OrgID: "00000000-0000-0000-0000-000000000002",
		RunID: "00000000-0000-0000-0000-000000000003", WorkerGroupID: "group-1",
		WorkerInstanceID: "00000000-0000-0000-0000-000000000004", WorkerEpoch: 5, LeaseSequence: 6, SnapshotVersion: 9,
		RuntimeInstanceID: "00000000-0000-0000-0000-000000000005",
		ProtocolVersion:   "helmr.worker.v0", AttemptNumber: 8,
	}
	if _, err := parseWorkerRunLease(lease); err == nil {
		t.Fatal("expected missing network slot fence to fail")
	}
}
