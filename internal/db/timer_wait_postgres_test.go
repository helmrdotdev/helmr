package db

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTimerWaitRegistrationAndHotCompletion(t *testing.T) {
	ctx := context.Background()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, ctx, fixture.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.RunID)

	var runVersion int64
	if err := fixture.Pool.QueryRow(
		ctx,
		`SELECT revision FROM runs WHERE id = $1`,
		work.RunID,
	).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-time.Millisecond)
	wait, err := queries.RegisterTimerRunWait(ctx, RegisterTimerRunWaitParams{
		ID:                             pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:                  pgvalue.UUID(fixture.EnvironmentID),
		DueAt:                          pgvalue.Timestamptz(dueAt),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30_000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("timer-wait")),
		AttemptNumber:                  1,
		CurrentRunLeaseID:              pgvalue.UUID(work.LeaseID),
		Metadata:                       []byte(`{}`), Tags: []string{},
		RunID:                   pgvalue.UUID(work.RunID),
		ExpectedRunningRevision: runVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wait.Kind != WaitKindTimer || !wait.DueAt.Valid || wait.TimeoutAt.Valid ||
		wait.ConditionStatus != WaitStatusPending || wait.SuspensionStatus != RunWaitStatusHot {
		t.Fatalf("registered timer Wait = %+v", wait)
	}
	completed, err := queries.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{
		ID: wait.ID, RunID: wait.RunID,
		ExpectedRunRevision: wait.ExpectedRunRevision,
		CurrentRunLeaseID:   wait.CurrentRunLeaseID,
		AttemptNumber:       wait.AttemptNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	var status RunStatus
	if err := fixture.Pool.QueryRow(
		ctx,
		`SELECT status FROM runs WHERE id = $1`,
		work.RunID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if completed.ConditionStatus != WaitStatusCompleted ||
		completed.SuspensionStatus != RunWaitStatusReleased ||
		completed.CompletedTurnID.Valid ||
		status != RunStatusRunning {
		t.Fatalf("completed timer Wait = %+v run=%s", completed, status)
	}
	if _, err := fixture.Pool.Exec(ctx, `
		UPDATE run_waits
		   SET completed_turn_id = $2
		 WHERE id = $1
	`, wait.ID, uuid.NewV7()); err == nil {
		t.Fatal("timer Wait accepted a completed Turn")
	}
}
