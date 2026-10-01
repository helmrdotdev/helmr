package dispatch_test

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
)

func TestIdleComputerReconcilerRequestsCapture(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	count, err := authority.ReconcileComputerInstances(t.Context(), 10)
	if err != nil || count != 1 {
		t.Fatalf("reconcile=%d %v", count, err)
	}
	var captured bool
	err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='checkpointing' AND capture_checkpoint_id IS NOT NULL AND desired_version=$2 FROM computer_instances WHERE id=$1`, request.InstanceID, request.DesiredVersion+1).Scan(&captured)
	if err != nil || !captured {
		t.Fatalf("capture not scheduled: %v %v", captured, err)
	}
	count, err = authority.ReconcileComputerInstances(t.Context(), 10)
	if err != nil || count != 0 {
		t.Fatalf("repeated reconcile=%d %v", count, err)
	}
}

func TestIdleComputerReconcilerAdvancesPastRejectedCandidate(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, request.InstanceID)
	newer := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now() WHERE id=$1`, newer.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE id=$1`, newer.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET last_activity_at=clock_timestamp()-interval '1 minute' WHERE id=(SELECT computer_id FROM run_leases WHERE id=$1)`, newer.LeaseID)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	count, err := authority.ReconcileComputerInstances(t.Context(), 1)
	if err != nil || count != 1 {
		t.Fatalf("reconcile behind rejected oldest candidate = %d %v", count, err)
	}
	var captured bool
	err = f.Pool.QueryRow(t.Context(), `SELECT capture_checkpoint_id IS NOT NULL FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, newer.LeaseID).Scan(&captured)
	if err != nil || !captured {
		t.Fatalf("newer Computer starved: %v %v", captured, err)
	}
}
