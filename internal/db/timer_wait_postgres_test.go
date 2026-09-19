package db

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTimerWaitRegistrationAndHotCompletion(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)

	var runVersion int64
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT revision FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-time.Millisecond)
	wait, err := fixture.queries.RegisterTimerRunWait(ctx, RegisterTimerRunWaitParams{
		ID:                             pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:                  pgvalue.UUID(fixture.environmentID),
		DueAt:                          pgvalue.Timestamptz(dueAt),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30_000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("timer-wait")),
		AttemptNumber:                  1,
		CurrentRunLeaseID:              pgvalue.UUID(work.leaseID),
		CheckpointDueAt:                pgvalue.Timestamptz(time.Now().Add(time.Second)),
		ResumeAttachID:                 pgvalue.UUID(uuid.NewV7()),
		Metadata:                       []byte(`{}`), Tags: []string{},
		RunID:                   pgvalue.UUID(work.runID),
		ExpectedRunningRevision: runVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wait.Kind != WaitKindTimer || !wait.DueAt.Valid || wait.TimeoutAt.Valid ||
		wait.ConditionStatus != WaitStatusPending || wait.SuspensionStatus != RunWaitStatusHot {
		t.Fatalf("registered timer Wait = %+v", wait)
	}
	completed, err := fixture.queries.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{
		ID: wait.ID, RunID: wait.RunID,
		ExpectedRunRevision: wait.ExpectedRunRevision,
		CurrentRunLeaseID:   wait.CurrentRunLeaseID,
		AttemptNumber:       wait.AttemptNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	var status RunStatus
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT status FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if completed.ConditionStatus != WaitStatusCompleted ||
		completed.SuspensionStatus != RunWaitStatusReleased ||
		completed.CompletedTurnID.Valid ||
		status != RunStatusRunning {
		t.Fatalf("completed timer Wait = %+v run=%s", completed, status)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE run_waits
		   SET completed_turn_id = $2
		 WHERE id = $1
	`, wait.ID, uuid.NewV7()); err == nil {
		t.Fatal("timer Wait accepted a completed Actor record")
	}
}
