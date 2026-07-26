package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type taskCompletionWork struct {
	workspaceID       uuid.UUID
	baseVersionID     uuid.UUID
	physicalVersionID uuid.UUID
	runtimeID         uuid.UUID
	mountID           uuid.UUID
	workspaceLeaseID  uuid.UUID
}

func TestTaskCompletionQueriesCommitReplayAndRollback(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	beginTaskCompletionFinalization(t, ctx, fixture, work)
	fingerprint := "sha256:fresh-task-completion"

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
		ExpectedHeadVersionID: pgvalue.UUID(authority.baseVersionID),
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
		RunLeaseID: pgvalue.UUID(work.leaseID), RunID: pgvalue.UUID(work.runID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: 1, LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
	})
	if err != nil || !replay.Valid || replay.String != fingerprint {
		t.Fatalf("replay = %v, %v, want %q", replay, err, fingerprint)
	}
	var runStatus RunStatus
	var leaseState RunLeaseState
	var attemptOutcome pgtype.Text
	var ownerRunID pgtype.UUID
	var eventCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_leases.state, run_attempts.terminal_outcome,
		       workspaces.owner_run_id,
		       (SELECT count(*) FROM telemetry_outbox WHERE run_id = runs.id AND kind = 'run.completed')
		  FROM runs
		  JOIN run_leases ON run_leases.id = $1
		  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
		  JOIN workspaces ON workspaces.id = runs.workspace_id
		 WHERE runs.id = $2
	`, work.leaseID, work.runID).Scan(
		&runStatus, &leaseState, &attemptOutcome, &ownerRunID, &eventCount,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusSucceeded || leaseState != RunLeaseStateCompleted ||
		!attemptOutcome.Valid || attemptOutcome.String != "succeeded" || ownerRunID.Valid || eventCount != 1 {
		t.Fatalf("terminal state = run %s lease %s attempt %v owner %v events %d", runStatus, leaseState, attemptOutcome, ownerRunID, eventCount)
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
	completeTaskTerminalRows(t, ctx, queries, rollbackWork, rollbackAuthority, completedAt, "sha256:rollback", true)
	_, err = queries.ReleaseTaskWorkspaceLease(ctx, ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: completedAt, ID: pgvalue.UUID(rollbackAuthority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(rollbackAuthority.workspaceID), WorkspaceMountID: pgvalue.UUID(rollbackAuthority.mountID),
		RuntimeInstanceID: pgvalue.UUID(rollbackAuthority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(rollbackWork.leaseID),
		BaseVersionID: pgvalue.UUID(rollbackAuthority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 2, MountFencingGeneration: 2,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched Workspace fence error = %v, want no rows", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_leases.state, run_attempts.terminal_outcome
		  FROM runs
		  JOIN run_leases ON run_leases.id = $1
		  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
		 WHERE runs.id = $2
	`, rollbackWork.leaseID, rollbackWork.runID).Scan(&runStatus, &leaseState, &attemptOutcome); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusRunning || leaseState != RunLeaseStateFinalizing || attemptOutcome.Valid {
		t.Fatalf("rollback state = run %s lease %s attempt %v", runStatus, leaseState, attemptOutcome)
	}
}

func TestRestoredTaskFailureRollsBackPhysicalFrontier(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	restoredVersionID := uuid.Must(uuid.NewV7())
	artifactID := uuid.Must(uuid.NewV7())
	digest := runLeaseTestDigest("restored-task-frontier")
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/octet-stream')
	`, fixture.orgID, digest)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind,
			size_bytes, media_type, created_by_worker_instance_id
		) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, 'application/octet-stream', $6)
	`, artifactID, fixture.orgID, fixture.projectID, fixture.environmentID, digest, fixture.workerID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id,
			parent_version_id, artifact_id, artifact_kind, kind, content_digest,
			size_bytes, entry_count, state, source_workspace_lease_id,
			ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'workspace_version', 'user', $9,
			1, 1, 'committed', $10, 1, 1, now()
		)
	`, restoredVersionID, runLeasePublicID(t, publicid.WorkspaceVersion), fixture.orgID,
		fixture.projectID, fixture.environmentID, authority.workspaceID, authority.baseVersionID,
		artifactID, digest, authority.workspaceLeaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE workspace_mounts SET materialized_version_id = $1 WHERE id = $2
	`, restoredVersionID, authority.mountID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases SET base_version_id = $1 WHERE id = $2
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
		NewVersionID: pgvalue.UUID(authority.baseVersionID), CompletedAt: completedAt,
		ID: pgvalue.UUID(authority.mountID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), RuntimeInstanceID: pgvalue.UUID(authority.runtimeID),
		BaseVersionID: pgvalue.UUID(restoredVersionID), MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	completeTaskAttemptQueries(t, ctx, queries, work, authority, completedAt, "sha256:restored-failure", false)
	if _, err := queries.ReleaseTaskWorkspaceOwner(ctx, ReleaseTaskWorkspaceOwnerParams{
		CompletedAt: completedAt,
		ID:          pgvalue.UUID(authority.workspaceID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		RunID: pgvalue.UUID(work.runID), OwnershipGeneration: 1, WriterGeneration: 1,
		ExpectedHeadVersionID: pgvalue.UUID(authority.baseVersionID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.FinishTaskRun(ctx, FinishTaskRunParams{
		Status: RunStatusFailed, ReasonCode: pgvalue.Text("task_failed"), Error: []byte(`{"message":"failed"}`),
		CompletedAt: completedAt, ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		AttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var mountedVersionID, headVersionID uuid.UUID
	var ownerRunID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT workspace_mounts.materialized_version_id, workspaces.head_version_id, workspaces.owner_run_id
		  FROM workspace_mounts
		  JOIN workspaces ON workspaces.id = workspace_mounts.workspace_id
		 WHERE workspace_mounts.id = $1
	`, authority.mountID).Scan(&mountedVersionID, &headVersionID, &ownerRunID); err != nil {
		t.Fatal(err)
	}
	if mountedVersionID != authority.baseVersionID || headVersionID != authority.baseVersionID || ownerRunID.Valid {
		t.Fatalf("restored rollback = mount %s head %s owner %v, want base %s and no owner", mountedVersionID, headVersionID, ownerRunID, authority.baseVersionID)
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
	completeTaskAttemptQueries(t, ctx, queries, work, authority, completedAt, "sha256:retry", false)
	if _, err := queries.CreateTaskRetryAttempt(ctx, CreateTaskRetryAttemptParams{
		Number: 2, RunID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DelayTaskRunRetry(ctx, DelayTaskRunRetryParams{
		NextAttemptNumber: 2, CompletedAt: completedAt, RetryAt: completedAt,
		ID: pgvalue.UUID(work.runID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		PreviousAttemptNumber: 1, RunLeaseID: pgvalue.UUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var delayedStateVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&delayedStateVersion); err != nil {
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
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			rows, err := fixture.queries.ReadyRunRetries(ctx, ReadyRunRetriesParams{
				OutboxMessageIds: pgvalue.NewUUIDv7Batch(1),
				RowLimit:         1,
			})
			results <- result{rows: rows, err: err}
		}()
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
	if rows, err := fixture.queries.ReadyRunRetries(ctx, ReadyRunRetriesParams{
		OutboxMessageIds: pgvalue.NewUUIDv7Batch(1),
		RowLimit:         1,
	}); err != nil || len(rows) != 0 {
		t.Fatalf("replayed readiness = %d rows, %v, want no rows", len(rows), err)
	}

	var status RunStatus
	var attemptNumber int32
	var stateVersion int64
	var retryAt pgtype.Timestamptz
	var outboxCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.current_attempt_number, runs.state_version, runs.retry_at,
		       (SELECT count(*) FROM outbox_messages
		         WHERE topic = 'run.admit' AND payload->>'runId' = runs.id::text)
		  FROM runs
		 WHERE runs.id = $1
	`, work.runID).Scan(&status, &attemptNumber, &stateVersion, &retryAt, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if status != RunStatusQueued || attemptNumber != 2 || stateVersion != delayedStateVersion+1 || retryAt.Valid || outboxCount != 1 {
		t.Fatalf("ready retry = status %s attempt %d version %d retry_at %v outbox %d", status, attemptNumber, stateVersion, retryAt, outboxCount)
	}
}

func startTaskCompletionWork(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
) taskCompletionWork {
	t.Helper()
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'running', started_at = claimed_at
		 WHERE id = $1 AND state = 'starting'
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'running', state_version = state_version + 1,
		       started_at = (SELECT started_at FROM run_leases WHERE id = $1),
		       active_started_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE id = $2 AND status = 'queued' AND current_run_lease_id = $1
	`, work.leaseID, work.runID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_attempts
		   SET entrypoint_entered_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE run_id = $2 AND number = 1 AND entrypoint_entered_at IS NULL
	`, work.leaseID, work.runID)
	var authority taskCompletionWork
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, runs.base_workspace_version_id,
		       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
		       workspace_leases.id, workspace_leases.base_version_id
		  FROM runs
		  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&authority.workspaceID, &authority.baseVersionID, &authority.runtimeID,
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
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET active_elapsed_ms = active_elapsed_ms
		           + floor(extract(epoch FROM (now() - active_started_at)) * 1000)::bigint,
		       active_started_at = NULL,
		       state_version = state_version + 1,
		       updated_at = now()
		 WHERE id = $1
		   AND current_run_lease_id = $2
		   AND status = 'running'
		   AND active_started_at IS NOT NULL
	`, work.runID, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'finalizing',
		       finalization_operation_id = $2,
		       finalization_kind = 'reset',
		       finalization_started_at = now(),
		       finalization_request_fingerprint = 'test-finalization'
		 WHERE id = $1
		   AND state = 'running'
	`, work.leaseID, uuid.Must(uuid.NewV7()))
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
		BaseVersionID: pgvalue.UUID(authority.physicalVersionID), OwnershipGeneration: 1,
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
	leaseState := RunLeaseStateFailed
	outcome := "failed"
	reason := "task_failed"
	var terminalError []byte
	if succeeded {
		leaseState = RunLeaseStateCompleted
		outcome = "succeeded"
		reason = "completed"
	} else {
		terminalError = []byte(`{"message":"failed"}`)
	}
	if _, err := queries.CompleteTaskRunLease(ctx, CompleteTaskRunLeaseParams{
		State: leaseState, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason),
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
