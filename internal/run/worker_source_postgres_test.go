package run

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// receipt is the worker's unparsed receipt for the fixture's source lease.
func (f liveSourceFixture) receipt() SourceReceipt {
	return SourceReceipt{
		Worker: workergroup.HostPrincipal{
			HostID: pgvalue.MustUUIDValue(f.fence.WorkerHostID), GroupID: pgvalue.MustUUIDValue(f.fence.WorkerGroupID), Epoch: f.fence.WorkerEpoch,
			HostClaimVersion: f.fence.HostClaimVersion, GroupClaimVersion: f.fence.GroupClaimVersion,
		},
		LeaseID: pgvalue.UUIDString(f.fence.LeaseID), LeaseSequence: f.fence.LeaseSequence,
	}
}

func TestLockWorkerSourceLocksTheReceiptsLiveSource(t *testing.T) {
	f := newLiveSourceFixture(t)
	source, err := LockWorkerSource(t.Context(), f.Pool, f.receipt())
	if err != nil {
		t.Fatal(err)
	}
	if source.RunID() != pgvalue.UUID(f.work.RunID) || source.ComputerID() != pgvalue.UUID(f.source) || source.EnvironmentID() != pgvalue.UUID(f.EnvironmentID) {
		t.Fatalf("source Run=%s Computer=%s Environment=%s", pgvalue.UUIDString(source.RunID()), pgvalue.UUIDString(source.ComputerID()), pgvalue.UUIDString(source.EnvironmentID()))
	}
}

func TestLockWorkerSourceRejectsStaleAndMalformedReceipts(t *testing.T) {
	f := newLiveSourceFixture(t)
	stale := f.receipt()
	stale.LeaseSequence++
	if _, err := LockWorkerSource(t.Context(), f.Pool, stale); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale lease sequence = %v", err)
	}
	malformed := f.receipt()
	malformed.LeaseID = "not-a-lease"
	if _, err := LockWorkerSource(t.Context(), f.Pool, malformed); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("malformed receipt = %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=NULL WHERE run_id=$1 AND number=1`, f.work.RunID)
	if _, err := LockWorkerSource(t.Context(), f.Pool, f.receipt()); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("source that has not entered = %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1 AND number=1`, f.work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, f.fence.WorkerGroupID)
	if _, err := LockWorkerSource(t.Context(), f.Pool, f.receipt()); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale worker claims = %v", err)
	}
}
