package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorInputWaitAppendAndRegistrationOrdersConverge(t *testing.T) {
	for _, appendFirst := range []bool{false, true} {
		name := "registration-first"
		if appendFirst {
			name = "append-first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunLeaseClaimFixture(t, ctx)
			work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
			startTaskCompletionWork(t, ctx, fixture, work)

			var runVersion int64
			if err := fixture.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
				t.Fatal(err)
			}
			waitID := uuid.NewV7()
			turnID := uuid.NewV7()
			register := func() RunWait {
				wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
					ID: pgvalue.UUID(waitID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
					IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, SessionID: pgvalue.UUID(actorID),
					AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
					RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("actor-input-wait")), AttemptNumber: 1,
					ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true},
					CurrentRunLeaseID:             pgvalue.UUID(work.leaseID),
					CheckpointDueAt:               pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
					ResumeAttachID:                pgvalue.UUID(uuid.NewV7()), Metadata: []byte(`{}`), Tags: []string{},
					RunID: pgvalue.UUID(work.runID), ExpectedRunningRevision: runVersion,
				})
				if err != nil {
					t.Fatal(err)
				}
				return wait
			}
			appendRecord := func() SessionTurn {
				record, err := fixture.queries.EnqueueSessionTurn(ctx, EnqueueSessionTurnParams{
					EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
					ID: pgvalue.UUID(turnID), Data: []byte(`{"message":"ready"}`)})
				if err != nil {
					t.Fatal(err)
				}
				if record.Sequence != 3 {
					t.Fatalf("append = %+v", record)
				}
				return record
			}

			var wait RunWait
			var record SessionTurn
			if appendFirst {
				record = appendRecord()
				wait = register()
			} else {
				wait = register()
				record = appendRecord()
			}
			pending, err := fixture.queries.GetPendingActorInputRunWait(ctx, GetPendingActorInputRunWaitParams{
				EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
				RunID: pgvalue.UUID(work.runID), AttemptNumber: 1,
				AfterInputSequence: pgtype.Int8{Int64: 2, Valid: true},
			})
			if err != nil || pending.ID != wait.ID {
				t.Fatalf("pending Wait = %+v, %v", pending, err)
			}
			completed, err := fixture.queries.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{
				ConditionResult: []byte(`{"value":{"message":"ready"}}`), CompletedTurnID: record.ID,
				ID: pending.ID, RunID: pending.RunID, ExpectedRunRevision: pending.ExpectedRunRevision,
				CurrentRunLeaseID: pending.CurrentRunLeaseID, AttemptNumber: pending.AttemptNumber,
			})
			if err != nil {
				t.Fatal(err)
			}
			var status RunStatus
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, work.runID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if completed.ConditionStatus != WaitStatusCompleted || completed.SuspensionStatus != RunWaitStatusReleased ||
				completed.CompletedTurnID != record.ID ||
				status != RunStatusRunning {
				t.Fatalf("completion = %+v run=%s", completed, status)
			}
		})
	}
}

func TestSessionTurnEnqueueConcurrentSequences(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	type result struct {
		turn SessionTurn
		err  error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			turn, err := f.queries.EnqueueSessionTurn(ctx, EnqueueSessionTurnParams{
				EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(sessionID),
				ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`null`), SourceRunID: pgvalue.UUID(work.runID),
			})
			results <- result{turn, err}
		}()
	}
	start.Done()
	sequences := map[int64]bool{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.turn.SourceRunID != pgvalue.UUID(work.runID) || r.turn.Status != "queued" {
			t.Fatalf("turn=%+v", r.turn)
		}
		sequences[r.turn.Sequence] = true
	}
	if len(sequences) != 2 || !sequences[3] || !sequences[4] {
		t.Fatalf("sequences=%v", sequences)
	}
}

func TestSessionTurnEnqueueRollbackLeavesNoResidue(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	turnID, outboxID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)
	turn, err := q.EnqueueSessionTurn(ctx, EnqueueSessionTurnParams{
		EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(sessionID),
		ID: pgvalue.UUID(turnID), Data: []byte(`{"rollback":true}`), SourceRunID: pgvalue.UUID(work.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreateActorInputReconcileOutbox(ctx, CreateActorInputReconcileOutboxParams{
		ID: pgvalue.UUID(outboxID), EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(sessionID), TurnID: turn.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var next int64
	var turns, outbox int
	if err := f.pool.QueryRow(ctx, `SELECT next_input_sequence,
  (SELECT count(*) FROM session_turns WHERE id=$2),
  (SELECT count(*) FROM control_outbox WHERE id=$3)
  FROM sessions WHERE id=$1`, sessionID, turnID, outboxID).Scan(&next, &turns, &outbox); err != nil {
		t.Fatal(err)
	}
	if next != 3 || turns != 0 || outbox != 0 {
		t.Fatalf("rollback next=%d turns=%d outbox=%d", next, turns, outbox)
	}
}

func TestSessionTurnSequenceSafeIntegerBoundary(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	const maxSafeSequence int64 = 9_007_199_254_740_991
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET next_input_sequence=$2 WHERE id=$1`, sessionID, maxSafeSequence)
	enqueue := func() (SessionTurn, error) {
		return f.queries.EnqueueSessionTurn(ctx, EnqueueSessionTurnParams{
			EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(sessionID), ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{}`),
		})
	}
	turn, err := enqueue()
	if err != nil || turn.Sequence != maxSafeSequence {
		t.Fatalf("maximum=%+v err=%v", turn, err)
	}
	if _, err := enqueue(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exhausted=%v", err)
	}
	var next int64
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT next_input_sequence,(SELECT count(*) FROM session_turns WHERE session_id=$1 AND sequence=$2) FROM sessions WHERE id=$1`, sessionID, maxSafeSequence).Scan(&next, &count); err != nil {
		t.Fatal(err)
	}
	if next != maxSafeSequence+1 || count != 1 {
		t.Fatalf("next=%d turns=%d", next, count)
	}
}

func TestActorInputWaitTimeoutReleasesHotRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	startTaskCompletionWork(t, ctx, fixture, work)
	var runVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	wait, err := fixture.queries.RegisterActorInputRunWait(ctx, RegisterActorInputRunWaitParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		TimeoutAt:     pgvalue.Timestamptz(time.Now().Add(-time.Millisecond)),
		IdleTimeoutMs: pgtype.Int8{Int64: 30_000, Valid: true}, SessionID: pgvalue.UUID(actorID),
		AfterInputSequence:             pgtype.Int8{Int64: 2, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("actor-input-timeout")), AttemptNumber: 1,
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 2, Valid: true}, CurrentRunLeaseID: pgvalue.UUID(work.leaseID),
		CheckpointDueAt: pgvalue.Timestamptz(time.Now().Add(30 * time.Second)),
		ResumeAttachID:  pgvalue.UUID(uuid.NewV7()), Metadata: []byte(`{}`), Tags: []string{},
		RunID: pgvalue.UUID(work.runID), ExpectedRunningRevision: runVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := fixture.queries.ListPendingActorInputWaitTimeouts(ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ID != wait.ID {
		t.Fatalf("timeout candidates = %+v, %v", candidates, err)
	}
	failed, err := fixture.queries.FailHotRunWait(ctx, FailHotRunWaitParams{
		ReasonCode: pgvalue.Text("wait_timeout"), ConditionError: []byte(`{"code":"wait_timeout","retryable":false}`),
		ID: wait.ID, RunID: wait.RunID, ExpectedRunRevision: wait.ExpectedRunRevision,
		CurrentRunLeaseID: wait.CurrentRunLeaseID, AttemptNumber: wait.AttemptNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	var status RunStatus
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, work.runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if failed.ConditionStatus != WaitStatusFailed || failed.SuspensionStatus != RunWaitStatusReleased ||
		pgvalue.TextValue(failed.ConditionReasonCode) != "wait_timeout" || status != RunStatusRunning {
		t.Fatalf("timeout completion = %+v run=%s", failed, status)
	}
}

func TestActorInputClosingContinuationCASCreatesOneRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM sessions WHERE id = $1`, actorID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases
		   SET status = 'released', released_at = now(), terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'cancelled', terminal_at = now(), terminal_reason_code = 'test_idle'
		 WHERE id = $1
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'failed', current_run_lease_id = NULL,
		       terminal_at = now(),
		       failure = '{"code":"test_idle","message":"Test run failed","details":{}}'::jsonb
		 WHERE id = $1
	`, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions
		   SET current_run_id = NULL, committed_input_sequence = 2
		 WHERE id = $1
	`, actorID)
	input, err := fixture.queries.EnqueueSessionTurn(ctx, EnqueueSessionTurnParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
		ID: pgvalue.UUID(uuid.NewV7()), Data: []byte(`{"wake":true}`)})
	if err != nil || input.Sequence != 3 {
		t.Fatalf("wake input = %+v, %v", input, err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE sessions
		   SET status = 'closing', close_sequence = 3
		 WHERE id = $1
	`, actorID)

	type result struct {
		run CreateActorContinuationRunRow
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			run, err := fixture.queries.CreateActorContinuationRun(ctx, CreateActorContinuationRunParams{
				RunID:         pgvalue.UUID(uuid.NewV7()),
				QueueOriginAt: pgvalue.Timestamptz(time.Now().UTC()),
				TraceID:       pgvalue.Text("11111111111111111111111111111111"), RootSpanID: "2222222222222222",
				EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
				WorkspaceID: pgvalue.UUID(workspaceID), ExpectedRunGeneration: 1,
			})
			results <- result{run: run, err: err}
		}()
	}
	start.Done()
	var created CreateActorContinuationRunRow
	createdCount, noRowsCount := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			createdCount++
			created = result.run
		case errors.Is(result.err, pgx.ErrNoRows):
			noRowsCount++
		default:
			t.Fatal(result.err)
		}
	}
	if createdCount != 1 || noRowsCount != 1 || created.CauseKind != "continuation" ||
		created.SessionInputStartSequence.Int64 != 2 || created.SessionInputHighWatermark.Int64 != 3 {
		t.Fatalf("continuation CAS = created %d no-rows %d run %+v", createdCount, noRowsCount, created)
	}
	var actorCurrentRun uuid.UUID
	var runCount, attemptCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT sessions.current_run_id,
		       (SELECT count(*) FROM runs WHERE session_id = sessions.id AND cause_kind = 'continuation'),
		       (SELECT count(*) FROM run_attempts WHERE run_id = sessions.current_run_id)
		  FROM sessions WHERE sessions.id = $1
	`, actorID).Scan(&actorCurrentRun, &runCount, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if actorCurrentRun != pgvalue.MustUUIDValue(created.ID) || runCount != 1 || attemptCount != 1 {
		t.Fatalf("durable continuation = current %s runs %d attempts %d", actorCurrentRun, runCount, attemptCount)
	}
}
