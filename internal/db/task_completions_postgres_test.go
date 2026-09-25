package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type taskCompletionWork struct {
	workspaceID            uuid.UUID
	baseWorkspaceVersionID uuid.UUID
	physicalVersionID      uuid.UUID
	runtimeID              uuid.UUID
	mountID                uuid.UUID
	workspaceLeaseID       uuid.UUID
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
	completeTaskAttemptQueries(t, ctx, queries, work, authority, completedAt, fingerprint, true)
	if _, err := queries.ReleaseTaskWorkspaceOwner(ctx, ReleaseTaskWorkspaceOwnerParams{
		CompletedAt: completedAt,
		ID:          pgvalue.UUID(authority.workspaceID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		RunID: pgvalue.UUID(work.runID), OwnershipGeneration: 1, WriterGeneration: 1,
		ExpectedHeadVersionID: pgvalue.UUID(authority.baseWorkspaceVersionID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.FinishTaskRun(ctx, FinishTaskRunParams{
		Status: RunStatusSucceeded, Output: []byte(`{"result":"ok"}`), CompletedAt: completedAt,
		ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
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
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
	})
	if err != nil || !replay.Valid || replay.String != fingerprint {
		t.Fatalf("replay = %v, %v, want %q", replay, err, fingerprint)
	}
	var runStatus RunStatus
	var leaseStatus RunLeaseStatus
	var attemptOutcome pgtype.Text
	var ownerRunID pgtype.UUID
	var eventCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_leases.status, run_attempts.terminal_outcome,
		       computers.owner_run_id,
		       (SELECT count(*) FROM telemetry_outbox WHERE run_id = runs.id AND kind = 'run.completed')
		  FROM runs
		  JOIN run_leases ON run_leases.id = $1
		  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
		  JOIN computers ON computers.id = runs.workspace_id
		 WHERE runs.id = $2
	`, work.leaseID, work.runID).Scan(
		&runStatus, &leaseStatus, &attemptOutcome, &ownerRunID, &eventCount,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusSucceeded || leaseStatus != RunLeaseStatusCompleted ||
		!attemptOutcome.Valid || attemptOutcome.String != "succeeded" || ownerRunID.Valid || eventCount != 1 {
		t.Fatalf("terminal state = run %s lease %s attempt %v owner %v events %d", runStatus, leaseStatus, attemptOutcome, ownerRunID, eventCount)
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
	_, err = queries.ReleaseTaskWorkspaceLease(ctx, ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: completedAt, ID: pgvalue.UUID(rollbackAuthority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(rollbackAuthority.workspaceID), WorkspaceMountID: pgvalue.UUID(rollbackAuthority.mountID),
		RuntimeInstanceID: pgvalue.UUID(rollbackAuthority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(rollbackWork.leaseID),
		BaseWorkspaceVersionID: pgvalue.UUID(rollbackAuthority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 2, MountFencingGeneration: 2,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched Workspace fence error = %v, want no rows", err)
	}
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

func TestTaskFailureRetainsPhysicalFrontier(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) { testTaskFailureRetainsPhysicalFrontier(t, retry) })
	}
}

func testTaskFailureRetainsPhysicalFrontier(t *testing.T, retry bool) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	restoredVersionID := uuid.NewV7()
	artifactID := uuid.NewV7()
	digest := dbtest.Digest("restored-task-frontier")
	dbtest.MustExec(t, ctx, fixture.pool, `
		WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/octet-stream')
	`, fixture.orgID, digest)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind,
			size_bytes, media_type, created_by_worker_instance_id
		) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, 'application/octet-stream', $6)
	`, artifactID, fixture.orgID, fixture.projectID, fixture.environmentID, digest, fixture.workerID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO computer_versions (
			id, environment_id, computer_id,
			parent_version_id, root_pack_digest,
			logical_bytes, status, source_workspace_lease_id,
			ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, $4, $5,
			1, 'committed', $6, 1, 1, now()
		)
	`, restoredVersionID, fixture.environmentID, authority.workspaceID, authority.baseWorkspaceVersionID, digest, authority.workspaceLeaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE workspace_mounts SET materialized_version_id = $1 WHERE id = $2
	`, restoredVersionID, authority.mountID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases SET base_workspace_version_id = $1 WHERE id = $2
	`, restoredVersionID, authority.workspaceLeaseID)
	authority.physicalVersionID = restoredVersionID
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
	if _, err := queries.UpdateTaskWorkspaceMountFrontier(ctx, UpdateTaskWorkspaceMountFrontierParams{
		NewVersionID: pgvalue.UUID(restoredVersionID), CompletedAt: completedAt,
		ID: pgvalue.UUID(authority.mountID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), RuntimeInstanceID: pgvalue.UUID(authority.runtimeID),
		BaseWorkspaceVersionID: pgvalue.UUID(restoredVersionID), MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	completeTaskAttemptQueries(t, ctx, queries, work, authority, completedAt, dbtest.Digest("retained-failure"), false)
	if retry {
		if _, err := queries.AdvanceTaskRetryWorkspaceHead(ctx, AdvanceTaskRetryWorkspaceHeadParams{
			ResultWorkspaceVersionID: pgvalue.UUID(restoredVersionID), CompletedAt: completedAt,
			WorkspaceID: pgvalue.UUID(authority.workspaceID), RunID: pgvalue.UUID(work.runID),
			ExpectedHeadVersionID: pgvalue.UUID(authority.baseWorkspaceVersionID), OwnershipGeneration: 1, WriterGeneration: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.CreateTaskRetryAttempt(ctx, CreateTaskRetryAttemptParams{
			ResultWorkspaceVersionID: pgvalue.UUID(restoredVersionID), Number: 2, RunID: pgvalue.UUID(work.runID),
			WorkspaceID: pgvalue.UUID(authority.workspaceID), PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.DelayTaskRunRetry(ctx, DelayTaskRunRetryParams{
			ResultWorkspaceVersionID: pgvalue.UUID(restoredVersionID), NextAttemptNumber: 2, CompletedAt: completedAt, RetryAt: completedAt,
			ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID), PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
	} else {

		if _, err := queries.ReleaseTaskWorkspaceOwner(ctx, ReleaseTaskWorkspaceOwnerParams{
			NewHeadVersionID: pgvalue.UUID(restoredVersionID),
			CompletedAt:      completedAt,
			ID:               pgvalue.UUID(authority.workspaceID), OrgID: pgvalue.UUID(fixture.orgID),
			ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
			RunID: pgvalue.UUID(work.runID), OwnershipGeneration: 1, WriterGeneration: 1,
			ExpectedHeadVersionID: pgvalue.UUID(authority.baseWorkspaceVersionID),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.FinishTaskRun(ctx, FinishTaskRunParams{
			Status: RunStatusFailed, Failure: []byte(`{"code":"task_failed","message":"failed","details":{}}`),
			CompletedAt: completedAt, ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
			AttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var mountedVersionID, headVersionID uuid.UUID
	var ownerRunID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT workspace_mounts.materialized_version_id, computers.head_version_id, computers.owner_run_id
		  FROM workspace_mounts
		  JOIN computers ON computers.id = workspace_mounts.workspace_id
		 WHERE workspace_mounts.id = $1
	`, authority.mountID).Scan(&mountedVersionID, &headVersionID, &ownerRunID); err != nil {
		t.Fatal(err)
	}
	if mountedVersionID != restoredVersionID || headVersionID != restoredVersionID || ownerRunID.Valid != retry {
		t.Fatalf("retained failure = mount %s head %s owner %v, want retained %s with expected ownership", mountedVersionID, headVersionID, ownerRunID, restoredVersionID)
	}
	if retry {
		var runBase, attemptBase uuid.UUID
		if err := fixture.pool.QueryRow(ctx, `SELECT runs.base_workspace_version_id, run_attempts.base_workspace_version_id FROM runs JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 2 WHERE runs.id = $1`, work.runID).Scan(&runBase, &attemptBase); err != nil {
			t.Fatal(err)
		}
		if runBase != restoredVersionID || attemptBase != restoredVersionID {
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
	completeTaskAttemptQueries(t, ctx, queries, work, authority, completedAt, dbtest.Digest("retry"), false)
	if _, err := queries.CreateTaskRetryAttempt(ctx, CreateTaskRetryAttemptParams{
		ResultWorkspaceVersionID: pgvalue.UUID(authority.baseWorkspaceVersionID),
		Number:                   2, RunID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DelayTaskRunRetry(ctx, DelayTaskRunRetryParams{
		ResultWorkspaceVersionID: pgvalue.UUID(authority.baseWorkspaceVersionID),
		NextAttemptNumber:        2, CompletedAt: completedAt, RetryAt: completedAt,
		ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
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
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, runs.base_workspace_version_id,
		       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
		       workspace_leases.id, workspace_leases.base_workspace_version_id
		  FROM runs
		  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&authority.workspaceID, &authority.baseWorkspaceVersionID, &authority.runtimeID,
		&authority.mountID, &authority.workspaceLeaseID, &authority.physicalVersionID,
	); err != nil {
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

func completeTaskAttemptQueries(
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
	completeTaskTerminalRows(t, ctx, queries, work, authority, completedAt, fingerprint, succeeded)
	if _, err := queries.ReleaseTaskWorkspaceLease(ctx, ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: completedAt, ID: pgvalue.UUID(authority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), WorkspaceMountID: pgvalue.UUID(authority.mountID),
		RuntimeInstanceID: pgvalue.UUID(authority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(work.leaseID),
		BaseWorkspaceVersionID: pgvalue.UUID(authority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 1, MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
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
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: 1, LeaseSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteTaskAttempt(ctx, CompleteTaskAttemptParams{
		TerminalOutcome: pgvalue.Text(outcome), ReasonCode: pgvalue.Text(reason), Error: terminalError,
		CompletedAt: completedAt, RunID: pgvalue.UUID(work.runID), Number: 1,
		WorkspaceID: pgvalue.UUID(authority.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
}
