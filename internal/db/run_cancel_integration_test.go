package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCancelRunTerminalizesQueuedTaskSession(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	taskSessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	seedTaskSessionRun(t, ctx, pool, ids, taskSessionID)
	seedCurrentAttempt(t, ctx, pool, ids, db.RunAttemptStatusQueued)
	operation := seedCancelOperation(t, ctx, queries, ids, "user requested")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		RunID:       pgvalue.UUID(ids.runID),
		Reason:      "user requested",
		Force:       false,
		OperationID: operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.TaskSessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT task_sessions.status, task_sessions.current_run_id, task_session_runs.ended_at
		  FROM task_sessions
		  JOIN task_session_runs ON task_session_runs.org_id = task_sessions.org_id
		                   AND task_session_runs.task_session_id = task_sessions.id
		 WHERE task_sessions.id = $1
	`, taskSessionID).Scan(&sessionStatus, &currentRunID, &endedAt); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.TaskSessionStatusCancelled {
		t.Fatalf("session status = %s, want cancelled", sessionStatus)
	}
	if currentRunID.Valid {
		t.Fatalf("current_run_id should be cleared, got %v", currentRunID)
	}
	if !endedAt.Valid {
		t.Fatal("task_session_runs.ended_at was not set")
	}
}

func TestCancelRunLeavesExecutingTaskSessionForRelease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	taskSessionID, _, _ := seedRunningTaskSessionLease(t, ctx, pool, ids)
	seedTaskSessionRun(t, ctx, pool, ids, taskSessionID)
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		RunID:       pgvalue.UUID(ids.runID),
		Reason:      "interrupt",
		Force:       false,
		OperationID: operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.TaskSessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runExecutionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `
		SELECT task_sessions.status, task_sessions.current_run_id, task_session_runs.ended_at, runs.execution_status
		  FROM task_sessions
		  JOIN task_session_runs ON task_session_runs.org_id = task_sessions.org_id
		                   AND task_session_runs.task_session_id = task_sessions.id
		  JOIN runs ON runs.org_id = task_sessions.org_id
		           AND runs.id = task_sessions.current_run_id
		 WHERE task_sessions.id = $1
	`, taskSessionID).Scan(&sessionStatus, &currentRunID, &endedAt, &runExecutionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.TaskSessionStatusOpen {
		t.Fatalf("session status = %s, want open", sessionStatus)
	}
	if !currentRunID.Valid {
		t.Fatal("current_run_id should remain set while leased run is pending cancellation")
	}
	if endedAt.Valid {
		t.Fatal("task_session_runs.ended_at should remain unset until lease release")
	}
	if runExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("run execution status = %s, want pending_cancel", runExecutionStatus)
	}
}

func TestCancelTaskSessionLeavesPendingCancelRunForRelease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	taskSessionID, _, _ := seedRunningTaskSessionLease(t, ctx, pool, ids)
	seedTaskSessionRun(t, ctx, pool, ids, taskSessionID)
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		RunID:       pgvalue.UUID(ids.runID),
		Reason:      "interrupt",
		Force:       false,
		OperationID: operation.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelTaskSession(ctx, db.CancelTaskSessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(taskSessionID),
		Reason:        "interrupt",
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.TaskSessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runExecutionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `
		SELECT task_sessions.status, task_sessions.current_run_id, task_session_runs.ended_at, runs.execution_status
		  FROM task_sessions
		  JOIN task_session_runs ON task_session_runs.org_id = task_sessions.org_id
		                   AND task_session_runs.task_session_id = task_sessions.id
		  JOIN runs ON runs.org_id = task_sessions.org_id
		           AND runs.id = $2
		 WHERE task_sessions.id = $1
	`, taskSessionID, ids.runID).Scan(&sessionStatus, &currentRunID, &endedAt, &runExecutionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.TaskSessionStatusCancelled || currentRunID.Valid || !endedAt.Valid {
		t.Fatalf("session after cancel = status %s current %v ended %v, want cancelled/no-current/ended", sessionStatus, currentRunID, endedAt)
	}
	if runExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("run execution status = %s, want pending_cancel", runExecutionStatus)
	}
}

func TestDeadLetterRunQueueItemTerminalizesTaskSession(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	taskSessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	seedTaskSessionRun(t, ctx, pool, ids, taskSessionID)
	seedCurrentAttempt(t, ctx, pool, ids, db.RunAttemptStatusQueued)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1000, 1024, 4096, 1, $3, 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (run_id, org_id, status, queue_name, dispatch_message_id)
		VALUES ($1, $2, 'queued', 'default', 'dispatch-1')
	`, ids.runID, ids.orgID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.DeadLetterRunQueueItem(ctx, db.DeadLetterRunQueueItemParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		DispatchMessageID: pgtype.Text{String: "dispatch-1", Valid: true},
		LastError:         "dispatch retries exhausted",
		EventKind:         "run.dead_lettered",
		EventPayload:      []byte(`{"reason":"dispatch retries exhausted"}`),
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.TaskSessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runStatus db.RunStatus
	var terminalOutcome db.NullRunTerminalOutcome
	if err := pool.QueryRow(ctx, `
		SELECT task_sessions.status, task_sessions.current_run_id, task_session_runs.ended_at, runs.status, runs.terminal_outcome
		  FROM task_sessions
		  JOIN task_session_runs ON task_session_runs.org_id = task_sessions.org_id
		                   AND task_session_runs.task_session_id = task_sessions.id
		  JOIN runs ON runs.org_id = task_sessions.org_id
		           AND runs.id = $2
		 WHERE task_sessions.id = $1
	`, taskSessionID, ids.runID).Scan(&sessionStatus, &currentRunID, &endedAt, &runStatus, &terminalOutcome); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.TaskSessionStatusOpen {
		t.Fatalf("session status = %s, want open", sessionStatus)
	}
	if currentRunID != pgvalue.UUID(ids.runID) {
		t.Fatalf("current_run_id = %v, want %v", currentRunID, ids.runID)
	}
	if !endedAt.Valid {
		t.Fatal("task_session_runs.ended_at was not set")
	}
	if runStatus != db.RunStatusFailed || !terminalOutcome.Valid || terminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeDeadLettered {
		t.Fatalf("run terminal state = %s/%v, want failed/dead_lettered", runStatus, terminalOutcome)
	}
}

func seedCurrentAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, status db.RunAttemptStatus) {
	t.Helper()
	attemptID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, $4)
	`, attemptID, ids.orgID, ids.runID, status); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_attempt_id = $1,
		       current_attempt_number = 1
		 WHERE org_id = $2
		   AND id = $3
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
}

func seedTaskSessionRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, taskSessionID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_session_runs (
			id, org_id, project_id, environment_id, task_session_id, run_id, deployment_id, turn_index
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
	`, uuid.Must(uuid.NewV7()), ids.orgID, ids.projectID, ids.environmentID, taskSessionID, ids.runID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
}

func seedCancelOperation(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, reason string) db.RunOperation {
	t.Helper()
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:          pgvalue.UUID(ids.orgID),
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		RunID:          pgvalue.UUID(ids.runID),
		Kind:           db.RunOperationKindCancel,
		ActorKind:      "test",
		ActorID:        "test",
		Reason:         reason,
		Request:        []byte(`{}`),
		IdempotencyKey: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}
