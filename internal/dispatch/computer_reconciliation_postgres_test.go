package dispatch

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"testing"
	"time"
	"uuid"
)

func TestUnownedComputerRecoveryRetainsSourceWithoutStartingRuntime(t *testing.T) {
	f := newRunPlacementFixture(t)
	source := workspaceHeadVersion(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET owner_run_id=NULL,status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',recovery_id=$2,recovery_version_id=head_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
	for range 2 {
		if _, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	var status, dirty string
	var head uuid.UUID
	var runtimes int
	var pinned bool
	if err := f.pool.QueryRow(f.ctx, `SELECT status,dirty_state,head_version_id,recovery_payload_required,(SELECT count(*) FROM runtime_instances WHERE workspace_id=c.id) FROM computers c WHERE id=$1`, f.workspaceID).Scan(&status, &dirty, &head, &pinned, &runtimes); err != nil {
		t.Fatal(err)
	}
	if status != "active" || dirty != "clean" || head != uuid.UUID(source.Bytes) || !pinned || runtimes != 0 {
		t.Fatalf("repaired=%s/%s head=%s pinned=%v runtimes=%d", status, dirty, head, pinned, runtimes)
	}
}

func TestProcessRecoveryFailureSettlesAfterPhysicalReclaim(t *testing.T) {
	for _, code := range []string{"computer_recovery_exhausted", "computer_source_unavailable"} {
		t.Run(code, func(t *testing.T) {
			f := newRunPlacementFixture(t)
			process := createPendingWorkspaceExec(t, f)
			dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
			placement, err := f.authority.PlaceWorkspaceExec(f.ctx, ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(process), ExpectedRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if code == "computer_recovery_exhausted" {
				dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET recovery_preparation_count=8 WHERE id=$1`, f.workspaceID)
			} else {
				dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',recovery_failure='{"code":"computer_source_unavailable","message":"retained root missing","details":{}}' WHERE id=$1`, f.workspaceID)
			}
			dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, placement.RuntimeInstanceID)
			if _, err = f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
				t.Fatal(err)
			}
			var state string
			if err = f.pool.QueryRow(f.ctx, `SELECT status FROM workspace_processes WHERE id=$1`, process).Scan(&state); err != nil || state != "pending" {
				t.Fatalf("before reclaim=%s %v", state, err)
			}
			rt, err := runtimeDeadlineState(f, placement.RuntimeInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.New(f.pool).MarkRuntimeInstanceClosed(f.ctx, db.MarkRuntimeInstanceClosedParams{ID: rt.ID, WorkerInstanceID: rt.WorkerInstanceID, WorkerEpoch: rt.WorkerEpoch, DesiredVersion: rt.DesiredVersion, ExpectedObservedVersion: rt.ObservedVersion, ReasonCode: pgvalue.Text("preparation_failed"), CleanupProof: []byte(`{"method":"host_reconciled"}`)}); err != nil {
				t.Fatal(err)
			}
			// Hold placement's first resource and wait until reconciliation blocks on it.
			// Reconciliation must not already own the Computer that placement needs next.
			placementTx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer placementTx.Rollback(context.Background())
			var blocker int
			if err = placementTx.QueryRow(f.ctx, `SELECT pg_backend_pid() FROM workspace_processes WHERE id=$1 FOR UPDATE`, process).Scan(&blocker); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, e := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); done <- e }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err = f.pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("reconciliation did not reach process lock")
				}
				time.Sleep(5 * time.Millisecond)
			}
			var computerID uuid.UUID
			if err = placementTx.QueryRow(f.ctx, `SELECT id FROM computers WHERE id=$1 FOR UPDATE NOWAIT`, f.workspaceID).Scan(&computerID); err != nil {
				t.Fatalf("reconciliation inverted placement lock order: %v", err)
			}
			if err = placementTx.Rollback(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err = f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
					t.Fatal(err)
				}
			}
			var reason, computer, claim string
			if err = f.pool.QueryRow(f.ctx, `SELECT p.status,p.terminal_reason_code,c.status,i.status FROM workspace_processes p JOIN computers c ON c.id=p.workspace_id JOIN idempotency_claims i ON i.id=p.claim_id WHERE p.id=$1`, process).Scan(&state, &reason, &computer, &claim); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || reason != code || computer != "recovery_required" || claim != "failed" {
				t.Fatalf("terminal=%s %s %s %s", state, reason, computer, claim)
			}
		})
	}
}

func TestPendingProcessReusesComputerAfterStaleMountCleanup(t *testing.T) {
	f, process, mount := prepareClaimableWorkspaceExecMount(t)
	var runtimeID uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT runtime_instance_id FROM workspace_mounts WHERE id=$1`, mount).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET reservation_expires_at=now()-interval '1 second' WHERE id=$1`, runtimeID)
	if _, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	rt, err := runtimeDeadlineState(f, pgvalue.UUID(runtimeID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(f.pool).MarkRuntimeInstanceClosed(f.ctx, db.MarkRuntimeInstanceClosedParams{ID: rt.ID, WorkerInstanceID: rt.WorkerInstanceID, WorkerEpoch: rt.WorkerEpoch, DesiredVersion: rt.DesiredVersion, ExpectedObservedVersion: rt.ObservedVersion, ReasonCode: pgvalue.Text("reservation_expired"), CleanupProof: []byte(`{"method":"host_reconciled"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = f.pool.QueryRow(f.ctx, `SELECT status FROM workspace_processes WHERE id=$1`, process).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("pending command changed=%s %v", state, err)
	}
	placement, err := f.authority.PlaceWorkspaceExec(f.ctx, ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(process), ExpectedRevision: 1})
	if err != nil || !placement.RuntimeInstanceID.Valid || placement.RuntimeInstanceID == pgvalue.UUID(runtimeID) {
		t.Fatalf("new placement=%+v %v", placement, err)
	}
}
