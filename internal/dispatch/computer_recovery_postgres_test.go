package dispatch

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestComputerRecoveryPreparationBudget(t *testing.T) {
	f := newRunPlacementFixture(t)
	root := workspaceHeadVersion(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,
		recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
	candidate := ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1}
	for count := 1; count <= 8; count++ {
		placement, err := f.authority.PlaceReadyRun(f.ctx, candidate)
		if err != nil {
			t.Fatalf("attempt %d: %v", count, err)
		}
		repeated, err := f.authority.PlaceReadyRun(f.ctx, candidate)
		if err != nil || repeated.RuntimeInstanceID != placement.RuntimeInstanceID {
			t.Fatalf("reservation replay: %+v %v", repeated, err)
		}
		var got int
		if err = f.pool.QueryRow(f.ctx, `SELECT recovery_preparation_count FROM computers WHERE id=$1`, f.workspaceID).Scan(&got); err != nil || got != count {
			t.Fatalf("count=%d want=%d err=%v", got, count, err)
		}
		rt, err := runtimeDeadlineState(f, placement.RuntimeInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate physical cleanup before permitting another reservation.
		dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, rt.ID)
		if n, err := f.authority.RecoverExpiredRuntimeReservations(f.ctx, 10); err != nil || n != 1 {
			t.Fatalf("preparation expiry: %d %v", n, err)
		}
		var runFailures int
		if err := f.pool.QueryRow(f.ctx, `SELECT runtime_preparation_count FROM runs WHERE id=$1`, f.runID).Scan(&runFailures); err != nil || runFailures != 0 {
			t.Fatalf("double charged Run: %d %v", runFailures, err)
		}
		rt, err = runtimeDeadlineState(f, rt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.New(f.pool).MarkRuntimeInstanceClosed(f.ctx, db.MarkRuntimeInstanceClosedParams{
			ID: rt.ID, WorkerInstanceID: rt.WorkerInstanceID, WorkerEpoch: rt.WorkerEpoch, DesiredVersion: rt.DesiredVersion,
			ExpectedObservedVersion: rt.ObservedVersion, ReasonCode: pgvalue.Text("preparation_failed"), CleanupProof: []byte(`{"method":"host_reconciled"}`),
		}); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET next_recovery_preparation_at=now()+interval '1 hour' WHERE id=$1`, f.workspaceID)
		if _, err := f.authority.PlaceReadyRun(f.ctx, candidate); err == nil {
			t.Fatal("backoff bypassed")
		}
		// Resetting Run-local preparation state cannot reset the Computer budget.
		dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runs SET runtime_preparation_count=0,next_runtime_preparation_at=NULL WHERE id=$1`, f.runID)
		dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET next_recovery_preparation_at=now()-interval '1 second' WHERE id=$1`, f.workspaceID)
	}
	if _, err := f.authority.PlaceReadyRun(f.ctx, candidate); err == nil {
		t.Fatal("ninth preparation admitted")
	}
	var count int
	var source string
	if err := f.pool.QueryRow(f.ctx, `SELECT recovery_preparation_count,recovery_version_id::text FROM computers WHERE id=$1`, f.workspaceID).Scan(&count, &source); err != nil || count != 8 || source != pgvalue.UUIDString(root) {
		t.Fatalf("count=%d source=%s err=%v", count, source, err)
	}
	var live int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runtime_instances WHERE workspace_id=$1 AND reclaimed_at IS NULL`, f.workspaceID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("rejected reservation leaked: %d %v", live, err)
	}
}

func TestProcessRecoveryPreparationUsesComputerBudget(t *testing.T) {
	f := newRunPlacementFixture(t)
	process := createPendingWorkspaceExec(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
	candidate := ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(process), ExpectedRevision: 1}
	first, err := f.authority.PlaceWorkspaceExec(f.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.authority.PlaceWorkspaceExec(f.ctx, candidate)
	if err != nil || again.RuntimeInstanceID != first.RuntimeInstanceID {
		t.Fatalf("process replay: %+v %v", again, err)
	}
	var count int
	if err = f.pool.QueryRow(f.ctx, `SELECT recovery_preparation_count FROM computers WHERE id=$1`, f.workspaceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
