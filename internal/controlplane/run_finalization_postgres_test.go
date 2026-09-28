package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
	"time"
	"uuid"
)

func TestProgramQuiescenceReconcilesOnlyProvenLease(t *testing.T) {
	f, work, fence, completion := taskHTTPExecutionFixture(t)
	peer := f.AddRunLease(t, "assigned", time.Now())
	s := &Server{db: db.New(f.Pool), tx: f.Pool}
	worker := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: fence.HostClaimVersion, GroupClaimVersion: fence.GroupClaimVersion}
	request := workerapi.BeginRunFinalizationRequest{Lease: completion.Lease, OperationID: completion.OperationID, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: work.RunID.String(), AttemptNumber: 1, RunLeaseID: work.LeaseID.String()}}
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	stale := worker
	stale.WorkerEpoch++
	if _, err = s.beginRunFinalization(t.Context(), stale, request, parsed); err == nil {
		t.Fatal("stale Worker accepted")
	}
	var clean bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&clean); err != nil || !clean {
		t.Fatalf("stale proof changed reconciliation: %v %v", clean, err)
	}
	var first time.Time
	for n := 0; n < 2; n++ {
		if _, err = s.beginRunFinalization(t.Context(), worker, request, parsed); err != nil {
			t.Fatal(err)
		}
		var at time.Time
		if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = at
		} else if !at.Equal(first) {
			t.Fatal("replay changed proof timestamp")
		}
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL FROM run_leases WHERE id=$1`, peer.LeaseID).Scan(&clean); err != nil || !clean {
		t.Fatalf("peer reconciled: %v %v", clean, err)
	}
	terminal, err := parseTaskCompletionRequest(completion)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.completeTask(t.Context(), worker, completion, terminal); err != nil {
		t.Fatal(err)
	}
	var computerID uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.deleteComputer(t.Context(), computerDeleteRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, ComputerID: computerID, IdempotencyKey: "delete-completed-member"}); err != nil {
		t.Fatalf("completed proven member blocked Computer deletion: %v", err)
	}

}
