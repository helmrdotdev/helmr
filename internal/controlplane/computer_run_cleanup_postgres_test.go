package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
	"time"
)

func TestComputerRunCleanupRequiresTerminalScopeAndCurrentPhysicalOwner(t *testing.T) {
	f, work, _, _ := taskHTTPExecutionFixture(t)
	peer := f.AddRunLease(t, "assigned", time.Now())
	worker, r := instanceRenewalFixture(t, f, work)
	request := workerapi.ComputerRunCleanupRequest{EnvironmentID: r.EnvironmentID, ComputerInstanceID: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration}
	receipt := workerapi.ComputerRunReconcileRequest{ComputerRunCleanupRequest: request, ComputerRunCleanup: workerapi.ComputerRunCleanup{RunID: work.RunID.String(), RunLeaseID: work.LeaseID.String(), AttemptNumber: 1}}
	s := &Server{db: db.New(f.Pool), tx: f.Pool}
	result, err := s.getComputerRunCleanup(t.Context(), worker, request)
	if err != nil || result.Run != nil {
		t.Fatalf("active Run selected: %+v %v", result, err)
	}
	if err = s.reconcileComputerRun(t.Context(), worker, receipt); err == nil {
		t.Fatal("active Run reconciled")
	}
	if _, err = f.Pool.Exec(t.Context(), `UPDATE run_leases SET status='cancelled',terminal_at=clock_timestamp(),terminal_reason_code='run_cancelled' WHERE id=$1`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	result, err = s.getComputerRunCleanup(t.Context(), worker, request)
	if err != nil || result.Run == nil || *result.Run != receipt.ComputerRunCleanup {
		t.Fatalf("cancelled Run missing: %+v %v", result, err)
	}
	for _, alter := range []func(*workerActor, *workerapi.ComputerRunReconcileRequest){
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { w.WorkerEpoch++ },
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { w.ClaimVersion++ },
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { w.GroupClaimVersion++ },
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { r.WriterGeneration++ },
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { r.AttemptNumber++ },
		func(w *workerActor, r *workerapi.ComputerRunReconcileRequest) { r.RunID = peer.RunID.String() },
	} {
		w, bad := worker, receipt
		alter(&w, &bad)
		if err = s.reconcileComputerRun(t.Context(), w, bad); err == nil {
			t.Fatal("stale proof accepted")
		}
	}
	var first time.Time
	for n := 0; n < 2; n++ {
		if err = s.reconcileComputerRun(t.Context(), worker, receipt); err != nil {
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
	result, err = s.getComputerRunCleanup(t.Context(), worker, request)
	if err != nil || result.Run != nil {
		t.Fatalf("reconciled Run selected: %+v %v", result, err)
	}
	var untouched bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL AND status='assigned' FROM run_leases WHERE id=$1`, peer.LeaseID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("peer changed: %v %v", untouched, err)
	}
}
