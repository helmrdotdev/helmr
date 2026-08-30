package run

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTimerWaitReconcilerCompletesDueHotWait(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "starting", time.Now().Add(-time.Minute))

	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'running', started_at = claimed_at
		 WHERE id = $1 AND state = 'starting'
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'running', state_version = state_version + 1,
		       started_at = (SELECT started_at FROM run_leases WHERE id = $1),
		       active_started_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE id = $2 AND status = 'queued' AND current_run_lease_id = $1
	`, work.leaseID, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_attempts
		   SET entrypoint_entered_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE run_id = $2 AND number = 1 AND entrypoint_entered_at IS NULL
	`, work.leaseID, work.runID)

	var runningVersion int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT state_version FROM runs WHERE id = $1
	`, work.runID).Scan(&runningVersion); err != nil {
		t.Fatal(err)
	}
	wait, err := fixture.queries.RegisterTimerRunWait(ctx, db.RegisterTimerRunWaitParams{
		ID:                             pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:                  pgvalue.UUID(fixture.environmentID),
		DueAt:                          pgvalue.Timestamptz(time.Now().UTC().Add(-time.Second)),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30_000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("timer-reconcile")),
		AttemptNumber:                  1,
		CurrentRunLeaseID:              pgvalue.UUID(work.leaseID),
		CheckpointDueAt:                pgvalue.Timestamptz(time.Now().UTC().Add(time.Second)),
		ResumeAttachID:                 pgvalue.UUID(uuid.NewV7()),
		Metadata:                       []byte(`{}`),
		Tags:                           []string{},
		RunID:                          pgvalue.UUID(work.runID),
		ExpectedRunningStateVersion:    runningVersion,
	})
	if err != nil {
		t.Fatal(err)
	}

	reconciler, err := NewTimerWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reconciler.ReconcileDue(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}

	var runStatus db.RunStatus
	var runVersion int64
	var conditionState db.WaitState
	var suspensionState db.RunWaitState
	var waitVersion int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.state_version,
		       run_waits.condition_state, run_waits.suspension_state,
		       run_waits.expected_run_state_version
		  FROM runs
		  JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, wait.ID).Scan(
		&runStatus, &runVersion, &conditionState, &suspensionState, &waitVersion,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusRunning ||
		conditionState != db.WaitStateCompleted ||
		suspensionState != db.RunWaitStateReleased ||
		runVersion != waitVersion {
		t.Fatalf(
			"reconciled timer = run status %s version %d, condition %s suspension %s wait version %d",
			runStatus, runVersion, conditionState, suspensionState, waitVersion,
		)
	}

	resolved, err = reconciler.ReconcileDue(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 0 {
		t.Fatalf("replayed reconciliation resolved = %d, want 0", resolved)
	}
}
