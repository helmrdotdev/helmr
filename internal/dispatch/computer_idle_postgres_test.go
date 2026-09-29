package dispatch_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
)

func TestIdleComputerCaptureRequiresEveryMemberDue(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         bool
	}{
		{"all due", "", true},
		{"peer still warm", `UPDATE run_waits SET idle_timeout_ms=3600000 WHERE run_id=$1`, false},
		{"peer disables suspension", `UPDATE run_waits SET idle_timeout_ms=NULL WHERE run_id=$1`, false},
		{"peer condition ready", `UPDATE run_waits SET due_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, peer, request := dispatchtest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
			if tc.change != "" {
				dbtest.MustExec(t, t.Context(), f.Pool, tc.change, peer.RunID)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = dispatch.BeginIdleComputerCapture(t.Context(), tx, request)
			if tc.want {
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("capture = %v", err)
				}
				if err = tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
				var untouched bool
				err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='open' AND capture_checkpoint_id IS NULL AND desired_version=$2 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints WHERE id=$3) FROM computer_instances WHERE id=$1`, request.ComputerInstanceID, request.DesiredVersion, request.CheckpointID).Scan(&untouched)
				if err != nil || !untouched {
					t.Fatalf("rejected capture changed authority: %v %v", untouched, err)
				}
			}
		})
	}
}

func TestIdleComputerCaptureEmptyCooldown(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "recent", true: "idle"}[old], func(t *testing.T) {
			f, _, _, request := dispatchtest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, request.ComputerInstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET last_activity_at=clock_timestamp()-CASE WHEN $2 THEN interval '1 minute' ELSE interval '0 seconds' END WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, request.ComputerInstanceID, old)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = dispatch.BeginIdleComputerCapture(t.Context(), tx, request)
			if old && err != nil || !old && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("old=%v capture=%v", old, err)
			}
		})
	}
}

func TestIdleComputerReconcilerRequestsCapture(t *testing.T) {
	f, _, _, request := dispatchtest.Capture(t)
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
	err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='checkpointing' AND capture_checkpoint_id IS NOT NULL AND desired_version=$2 FROM computer_instances WHERE id=$1`, request.ComputerInstanceID, request.DesiredVersion+1).Scan(&captured)
	if err != nil || !captured {
		t.Fatalf("capture not scheduled: %v %v", captured, err)
	}
	count, err = authority.ReconcileComputerInstances(t.Context(), 10)
	if err != nil || count != 0 {
		t.Fatalf("repeated reconcile=%d %v", count, err)
	}
}

func TestIdleComputerReconcilerAdvancesPastRejectedCandidate(t *testing.T) {
	f, _, _, request := dispatchtest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET dirty_state='capture_failed' WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, request.ComputerInstanceID)
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
