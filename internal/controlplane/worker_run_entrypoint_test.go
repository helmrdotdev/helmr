package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestEnterRunEntrypointTransaction(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	worker := workergroup.HostPrincipal{GroupID: runtest.WorkerGroupID, HostID: f.WorkerID, Epoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&worker.HostClaimVersion, &worker.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: 1, HostClaimVersion: worker.HostClaimVersion, GroupClaimVersion: worker.GroupClaimVersion}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = run.ClaimExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	started, err := run.StartExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := workerapi.RunEntrypointRequest{Lease: workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}, EntrypointKind: started.Run().EntrypointKind, EntrypointDeclaredID: started.Run().EntrypointDeclaredID}
	wrong := request
	wrong.EntrypointDeclaredID = "different"
	if err = enterRunEntrypoint(t.Context(), f.Pool, worker, fence.LeaseID, wrong); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("wrong entrypoint: %v", err)
	}
	var entered bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at IS NOT NULL FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&entered); err != nil || entered {
		t.Fatalf("rejected entry mutated receipt: %v %v", entered, err)
	}
	if err = enterRunEntrypoint(t.Context(), f.Pool, worker, fence.LeaseID, request); err != nil {
		t.Fatal(err)
	}
	var original, timeAfterReplay time.Time
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err = enterRunEntrypoint(t.Context(), f.Pool, worker, fence.LeaseID, request); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&timeAfterReplay); err != nil || !original.Equal(timeAfterReplay) {
		t.Fatalf("receipt changed on replay: %v", err)
	}
	stale := worker
	stale.HostClaimVersion++
	if err = enterRunEntrypoint(t.Context(), f.Pool, stale, fence.LeaseID, request); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, started.Instance().ID)
	if err = enterRunEntrypoint(t.Context(), f.Pool, worker, fence.LeaseID, request); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("expired writer: %v", err)
	}
}
