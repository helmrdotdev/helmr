package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func sessionControlExecutionFixture(t *testing.T) (runtest.Fixture, [2]ExecutionFence, [2]uuid.UUID) {
	t.Helper()
	f, first, fence := executionClaimFixture(t)
	second := f.AddRunLease(t, "assigned", time.Now())
	sessions := [2]uuid.UUID{f.ConvertToActor(t, t.Context(), first, `{"enabled":false}`), f.ConvertToActor(t, t.Context(), second, `{"enabled":false}`)}
	fences := [2]ExecutionFence{fence, fence}
	fences[1].LeaseID = pgvalue.UUID(second.LeaseID)
	for _, fence := range fences {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		a, err := ClaimExecution(t.Context(), tx, fence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = StartExecution(t.Context(), tx, fence); err != nil {
			t.Fatal(err)
		}
		if err = EnterExecution(t.Context(), tx, fence, a.Run.EntrypointKind, a.Run.EntrypointDeclaredID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return f, fences, sessions
}

func TestSessionInterruptionLocksOwnedTargetScopes(t *testing.T) {
	f, fences, sessions := sessionControlExecutionFixture(t)
	child := f.AddRunLease(t, "running", time.Now())
	claim := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash("control-child"))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET cause_kind='child',parent_run_id=(SELECT current_run_id FROM sessions WHERE id=$2),parent_owns_lifecycle=true,claim_id=$3 WHERE id=$1`, child.RunID, sessions[1], claim)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, graph, err := LockLiveExecutionForSessionInterruption(t.Context(), tx, fences[0], pgvalue.UUID(sessions[1]))
	if err != nil || a.Lease.ID != fences[0].LeaseID || graph.currentRun == uuid.Nil() {
		t.Fatalf("authority=%v graph=%v error=%v", a.Lease.ID, graph.currentRun, err)
	}
	for _, query := range []string{
		`SELECT id FROM computers WHERE id=(SELECT computer_id FROM runs WHERE id=$1) FOR UPDATE NOWAIT`,
		`SELECT id FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE run_id=$1) FOR UPDATE NOWAIT`,
		`SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT`,
		`SELECT run_id FROM run_attempts WHERE run_id=$1 FOR UPDATE NOWAIT`,
		`SELECT id FROM run_leases WHERE run_id=$1 FOR UPDATE NOWAIT`,
	} {
		probe, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = probe.Exec(t.Context(), query, child.RunID)
		_ = probe.Rollback(context.Background())
		if err == nil {
			t.Fatalf("target child scope was not locked: %s", query)
		}
	}
}

func TestReciprocalSessionInterruptionAuthority(t *testing.T) {
	f, fences, sessions := sessionControlExecutionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for index := range fences {
		go func() {
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback(context.Background())
			<-start
			_, _, err = LockLiveExecutionForSessionInterruption(ctx, tx, fences[index], pgvalue.UUID(sessions[1-index]))
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- err
		}()
	}
	close(start)
	for range fences {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, _, err = LockLiveExecutionForSessionInterruption(t.Context(), tx, fences[0], pgvalue.UUID(uuid.NewV7())); !errors.Is(err, ErrExecutionTargetNotFound) {
		t.Fatalf("missing target=%v", err)
	}
}

func TestSessionInterruptionRejectsTargetGenerationChangeDuringLock(t *testing.T) {
	f, fences, sessions := sessionControlExecutionFixture(t)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, sessions[1])
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := LockLiveExecutionForSessionInterruption(ctx, tx, fences[0], pgvalue.UUID(sessions[1]))
		done <- err
	}()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("control did not wait for target: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	dbtest.MustExec(t, t.Context(), blocker, `UPDATE sessions SET run_generation=run_generation+1 WHERE id=$1`, sessions[1])
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed target accepted: %v", err)
	}
}
