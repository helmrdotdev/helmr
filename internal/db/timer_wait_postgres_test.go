package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
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
		`SELECT state_version FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-time.Millisecond)
	wait, err := fixture.queries.RegisterTimerRunWait(ctx, RegisterTimerRunWaitParams{
		ID:                             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:                  pgvalue.UUID(fixture.environmentID),
		DueAt:                          pgvalue.Timestamptz(dueAt),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30_000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("timer-wait")),
		AttemptNumber:                  1,
		CurrentRunLeaseID:              pgvalue.UUID(work.leaseID),
		CheckpointDueAt:                pgvalue.Timestamptz(time.Now().Add(time.Second)),
		ResumeAttachID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Metadata:                       []byte(`{}`), Tags: []string{},
		RunID:                       pgvalue.UUID(work.runID),
		ExpectedRunningStateVersion: runVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wait.Kind != WaitKindTimer || !wait.DueAt.Valid || wait.TimeoutAt.Valid ||
		wait.ConditionState != WaitStatePending || wait.SuspensionState != RunWaitStateHot {
		t.Fatalf("registered timer Wait = %+v", wait)
	}
	completed, err := fixture.queries.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{
		ID: wait.ID, RunID: wait.RunID,
		ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
		CurrentRunLeaseID:       wait.CurrentRunLeaseID,
		AttemptNumber:           wait.AttemptNumber,
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
	if completed.ConditionState != WaitStateCompleted ||
		completed.SuspensionState != RunWaitStateReleased ||
		completed.CompletedActorRecordID.Valid ||
		completed.CompletedActorRecordDirection.Valid ||
		status != RunStatusRunning {
		t.Fatalf("completed timer Wait = %+v run=%s", completed, status)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE run_waits
		   SET completed_actor_record_id = $2,
		       completed_actor_record_direction = 'input'
		 WHERE id = $1
	`, wait.ID, uuid.Must(uuid.NewV7())); err == nil {
		t.Fatal("timer Wait accepted a completed Actor record")
	}
}
