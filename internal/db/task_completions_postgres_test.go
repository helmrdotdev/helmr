package db

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type taskCompletionWork struct {
	computerID                uuid.UUID
	baseComputerDiskVersionID uuid.UUID
	instanceID                uuid.UUID
}

func TestTaskCompletionQueriesCommitReplayAndRollback(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	beginTaskCompletionFinalization(t, ctx, fixture, work)
	fingerprint := dbtest.Digest("fresh-task-completion")

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	completedAt, err := queries.GetTaskCompletionTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeTaskTerminalRows(t, ctx, queries, work, authority, completedAt, fingerprint, true)
	if _, err := queries.FinishTaskRun(ctx, FinishTaskRunParams{
		Status: RunStatusSucceeded, Output: []byte(`{"result":"ok"}`), CompletedAt: completedAt,
		ID: pgvalue.UUID(work.runID), ComputerID: pgvalue.UUID(authority.computerID),
		AttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunEvent(ctx, AppendRunEventParams{
		Kind: "run.completed", Payload: []byte(`{}`),
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(work.runID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	replay, err := fixture.queries.GetTaskCompletionReplay(ctx, GetTaskCompletionReplayParams{
		RunLeaseID: pgvalue.UUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerHostID: pgvalue.UUID(fixture.workerID),
	})
	if err != nil || !replay.Valid || replay.String != fingerprint {
		t.Fatalf("replay = %v, %v, want %q", replay, err, fingerprint)
	}
	var runStatus RunStatus
	var leaseStatus RunLeaseStatus
	var attemptOutcome pgtype.Text
	var instanceReady bool
	var eventCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_leases.status, run_attempts.terminal_outcome,
		       (SELECT desired_state='ready' AND admission_state='open' AND reclaimed_at IS NULL FROM computer_instances WHERE id=run_leases.computer_instance_id),
		       (SELECT count(*) FROM telemetry_outbox WHERE run_id = runs.id AND kind = 'run.completed')
		  FROM runs
		  JOIN run_leases ON run_leases.id = $1
		  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
		  JOIN computers ON computers.id = runs.computer_id
		 WHERE runs.id = $2
	`, work.leaseID, work.runID).Scan(
		&runStatus, &leaseStatus, &attemptOutcome, &instanceReady, &eventCount,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusSucceeded || leaseStatus != RunLeaseStatusCompleted ||
		!attemptOutcome.Valid || attemptOutcome.String != "succeeded" || !instanceReady || eventCount != 1 {
		t.Fatalf("terminal state = run %s lease %s attempt %v owner %v events %d", runStatus, leaseStatus, attemptOutcome, instanceReady, eventCount)
	}

	rollbackWork := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	rollbackAuthority := startTaskCompletionWork(t, ctx, fixture, rollbackWork)
	beginTaskCompletionFinalization(t, ctx, fixture, rollbackWork)
	tx, err = fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries = New(tx)
	completedAt, err = queries.GetTaskCompletionTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeTaskTerminalRows(t, ctx, queries, rollbackWork, rollbackAuthority, completedAt, dbtest.Digest("rollback"), true)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_leases.status, run_attempts.terminal_outcome
		  FROM runs
		  JOIN run_leases ON run_leases.id = $1
		  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
		 WHERE runs.id = $2
	`, rollbackWork.leaseID, rollbackWork.runID).Scan(&runStatus, &leaseStatus, &attemptOutcome); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusRunning || leaseStatus != RunLeaseStatusFinalizing || attemptOutcome.Valid {
		t.Fatalf("rollback state = run %s lease %s attempt %v", runStatus, leaseStatus, attemptOutcome)
	}
}

func TestTaskFailurePreservesComputerAndInstance(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) { testTaskFailurePreservesComputerAndInstance(t, retry) })
	}
}

func testTaskFailurePreservesComputerAndInstance(t *testing.T, retry bool) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	retainedVersionID := authority.baseComputerDiskVersionID
	var before []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(c),to_jsonb(i)) FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, authority.instanceID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	beginTaskCompletionFinalization(t, ctx, fixture, work)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	completedAt, err := queries.GetTaskCompletionTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeTaskTerminalRows(t, ctx, queries, work, authority, completedAt, dbtest.Digest("retained-failure"), false)
	if retry {
		if _, err := queries.CreateTaskRetryAttempt(ctx, CreateTaskRetryAttemptParams{
			ResultComputerDiskVersionID: pgvalue.UUID(retainedVersionID), Number: 2, RunID: pgvalue.UUID(work.runID),
			ComputerID: pgvalue.UUID(authority.computerID), PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.DelayTaskRunRetry(ctx, DelayTaskRunRetryParams{
			ResultComputerDiskVersionID: pgvalue.UUID(retainedVersionID), NextAttemptNumber: 2, CompletedAt: completedAt, RetryAt: completedAt,
			ID: pgvalue.UUID(work.runID), ComputerID: pgvalue.UUID(authority.computerID), PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
	} else {

		if _, err := queries.FinishTaskRun(ctx, FinishTaskRunParams{
			Status: RunStatusFailed, Failure: []byte(`{"code":"task_failed","message":"failed","details":{}}`),
			CompletedAt: completedAt, ID: pgvalue.UUID(work.runID), ComputerID: pgvalue.UUID(authority.computerID),
			AttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var after []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(c),to_jsonb(i)) FROM computers c JOIN computer_instances i ON i.computer_id=c.id WHERE i.id=$1`, authority.instanceID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Task failure mutated Computer or physical writer")
	}
	if retry {
		var runBase, attemptBase uuid.UUID
		if err := fixture.pool.QueryRow(ctx, `SELECT runs.base_computer_disk_version_id, run_attempts.base_computer_disk_version_id FROM runs JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 2 WHERE runs.id = $1`, work.runID).Scan(&runBase, &attemptBase); err != nil {
			t.Fatal(err)
		}
		if runBase != retainedVersionID || attemptBase != retainedVersionID {
			t.Fatalf("retry discarded retained frontier: run=%s attempt=%s", runBase, attemptBase)
		}
	}

}

func TestReadyRunRetriesAdmitsOnceUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	beginTaskCompletionFinalization(t, ctx, fixture, work)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	completedAt, err := queries.GetTaskCompletionTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completeTaskTerminalRows(t, ctx, queries, work, authority, completedAt, dbtest.Digest("retry"), false)
	if _, err := queries.CreateTaskRetryAttempt(ctx, CreateTaskRetryAttemptParams{
		ResultComputerDiskVersionID: pgvalue.UUID(authority.baseComputerDiskVersionID),
		Number:                      2, RunID: pgvalue.UUID(work.runID), ComputerID: pgvalue.UUID(authority.computerID),
		PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DelayTaskRunRetry(ctx, DelayTaskRunRetryParams{
		ResultComputerDiskVersionID: pgvalue.UUID(authority.baseComputerDiskVersionID),
		NextAttemptNumber:           2, CompletedAt: completedAt, RetryAt: completedAt,
		ID: pgvalue.UUID(work.runID), ComputerID: pgvalue.UUID(authority.computerID),
		PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var delayedRevision int64
	if err := fixture.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id = $1`, work.runID).Scan(&delayedRevision); err != nil {
		t.Fatal(err)
	}

	if rows, err := fixture.queries.ReadyRunRetries(ctx, 1); err != nil || len(rows) != 0 {
		t.Fatalf("retry before process reconciliation = %d rows, %v, want no rows", len(rows), err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE run_leases SET process_reconciled_at = now() WHERE id = $1`, work.leaseID); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		rows []ReadyRunRetriesRow
		err  error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			rows, err := fixture.queries.ReadyRunRetries(ctx, 1)
			results <- result{rows: rows, err: err}
		})
	}
	close(start)
	wait.Wait()
	close(results)
	admissions := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		admissions += len(result.rows)
	}
	if admissions != 1 {
		t.Fatalf("concurrent admissions = %d, want 1", admissions)
	}
	if rows, err := fixture.queries.ReadyRunRetries(ctx, 1); err != nil || len(rows) != 0 {
		t.Fatalf("replayed readiness = %d rows, %v, want no rows", len(rows), err)
	}

	var status RunStatus
	var attemptNumber int32
	var revision int64
	var retryAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.current_attempt_number, runs.revision, runs.retry_at
		  FROM runs
		 WHERE runs.id = $1
	`, work.runID).Scan(&status, &attemptNumber, &revision, &retryAt); err != nil {
		t.Fatal(err)
	}
	if status != RunStatusQueued || attemptNumber != 2 || revision != delayedRevision+1 || retryAt.Valid {
		t.Fatalf("ready retry = status %s attempt %d version %d retry_at %v", status, attemptNumber, revision, retryAt)
	}
}

func startTaskCompletionWork(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
) taskCompletionWork {
	t.Helper()
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'running', started_at = claimed_at
		 WHERE id = $1 AND status = 'starting'
	`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'running', revision = revision + 1,
		       started_at = (SELECT started_at FROM run_leases WHERE id = $1),
		       active_started_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE id = $2 AND status = 'queued' AND current_run_lease_id = $1
	`, work.leaseID, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_attempts
		   SET entrypoint_entered_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE run_id = $2 AND number = 1 AND entrypoint_entered_at IS NULL
	`, work.leaseID, work.runID)
	var authority taskCompletionWork
	if err := fixture.pool.QueryRow(ctx, `SELECT r.computer_id,r.base_computer_disk_version_id,l.computer_instance_id FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, work.runID).Scan(&authority.computerID, &authority.baseComputerDiskVersionID, &authority.instanceID); err != nil {
		t.Fatal(err)
	}
	return authority
}

func beginTaskCompletionFinalization(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
) {
	t.Helper()
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET active_elapsed_ms = active_elapsed_ms
		           + floor(extract(epoch FROM (now() - active_started_at)) * 1000)::bigint,
		       active_started_at = NULL,
		       revision = revision + 1,
		       updated_at = now()
		 WHERE id = $1
		   AND current_run_lease_id = $2
		   AND status = 'running'
		   AND active_started_at IS NOT NULL
	`, work.runID, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'finalizing',
		       finalization_operation_id = $2,
		       finalization_started_at = now(),
		       finalization_request_fingerprint = 'sha256:6efa7ef866e15db96245ea5804c38662a1c3ef899643545704a867b61bdfc9eb'
		 WHERE id = $1
		   AND status = 'running'
	`, work.leaseID, uuid.NewV7())
}

func completeTaskTerminalRows(
	t *testing.T,
	ctx context.Context,
	queries *Queries,
	work runLeaseWork,
	authority taskCompletionWork,
	completedAt pgtype.Timestamptz,
	fingerprint string,
	succeeded bool,
) {
	t.Helper()
	leaseStatus := RunLeaseStatusFailed
	outcome := "failed"
	reason := "task_failed"
	var terminalError []byte
	if succeeded {
		leaseStatus = RunLeaseStatusCompleted
		outcome = "succeeded"
		reason = "completed"
	} else {
		terminalError = []byte(`{"message":"failed"}`)
	}
	if _, err := queries.CompleteTaskRunLease(ctx, CompleteTaskRunLeaseParams{
		Status: leaseStatus, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason),
		Error: terminalError, TerminalRequestFingerprint: pgvalue.Text(fingerprint),
		ID: pgvalue.UUID(work.leaseID), RunID: pgvalue.UUID(work.runID),
		ComputerID: pgvalue.UUID(authority.computerID), AttemptNumber: 1, LeaseSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteTaskAttempt(ctx, CompleteTaskAttemptParams{
		TerminalOutcome: pgvalue.Text(outcome), ReasonCode: pgvalue.Text(reason), Error: terminalError,
		CompletedAt: completedAt, RunID: pgvalue.UUID(work.runID), Number: 1,
		ComputerID: pgvalue.UUID(authority.computerID),
	}); err != nil {
		t.Fatal(err)
	}
}
