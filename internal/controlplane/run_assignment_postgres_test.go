package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestRunAssignmentProjectsClaimedExecution(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(map[bool]string{false: "Task", true: "Actor"}[actor], func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "assigned", time.Now())
			if actor {
				f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
			}
			fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
			if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
				t.Fatal(err)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			claimed, err := run.ClaimExecution(t.Context(), tx, fence)
			if err != nil {
				t.Fatal(err)
			}
			assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{run: claimed.Run, attempt: claimed.Attempt, runtime: claimed.Instance, runLease: claimed.Lease, computer: claimed.Computer})
			if err != nil {
				t.Fatal(err)
			}
			if assignment.ComputerInstanceID != pgvalue.UUIDString(claimed.Instance.ID) || assignment.WriterGeneration != claimed.Instance.WriterGeneration || assignment.BaseComputerDiskVersionID != pgvalue.UUIDString(claimed.Attempt.BaseComputerDiskVersionID) {
				t.Fatalf("assignment differs from claimed execution: %+v", assignment)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
