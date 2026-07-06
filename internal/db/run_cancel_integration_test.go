package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCancelRunTerminalizesQueuedRunAndLeavesSessionOpen(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	seedCurrentAttempt(t, ctx, pool, ids)
	operation := seedCancelOperation(t, ctx, queries, ids, "user requested")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "user requested",
		Force:         false,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.SessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT sessions.status, sessions.current_run_id, session_runs.ended_at
		  FROM sessions
		  JOIN session_runs ON session_runs.org_id = sessions.org_id
		                   AND session_runs.session_id = sessions.id
		 WHERE sessions.id = $1
	`, sessionID).Scan(&sessionStatus, &currentRunID, &endedAt); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.SessionStatusOpen {
		t.Fatalf("session status = %s, want open", sessionStatus)
	}
	if currentRunID != pgvalue.UUID(ids.runID) {
		t.Fatalf("current_run_id = %v, want %v", currentRunID, ids.runID)
	}
	if !endedAt.Valid {
		t.Fatal("session_runs.ended_at was not set")
	}
}

func TestCancelRunLeavesExecutingSessionForRelease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID, _, _ := seedRunningSessionLease(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "interrupt",
		Force:         false,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.SessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runExecutionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `
		SELECT sessions.status, sessions.current_run_id, session_runs.ended_at, runs.execution_status
		  FROM sessions
		  JOIN session_runs ON session_runs.org_id = sessions.org_id
		                   AND session_runs.session_id = sessions.id
		  JOIN runs ON runs.org_id = sessions.org_id
		           AND runs.id = sessions.current_run_id
		 WHERE sessions.id = $1
	`, sessionID).Scan(&sessionStatus, &currentRunID, &endedAt, &runExecutionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.SessionStatusOpen {
		t.Fatalf("session status = %s, want open", sessionStatus)
	}
	if !currentRunID.Valid {
		t.Fatal("current_run_id should remain set while leased run is pending cancellation")
	}
	if endedAt.Valid {
		t.Fatal("session_runs.ended_at should remain unset until lease release")
	}
	if runExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("run execution status = %s, want pending_cancel", runExecutionStatus)
	}
}

func TestCancelSessionLeavesPendingCancelRunForRelease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID, _, _ := seedRunningSessionLease(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "interrupt",
		Force:         false,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelSession(ctx, db.CancelSessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(sessionID),
		Reason:        "interrupt",
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.SessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runExecutionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `
		SELECT sessions.status, sessions.current_run_id, session_runs.ended_at, runs.execution_status
		  FROM sessions
		  JOIN session_runs ON session_runs.org_id = sessions.org_id
		                   AND session_runs.session_id = sessions.id
		  JOIN runs ON runs.org_id = sessions.org_id
		           AND runs.id = $2
		 WHERE sessions.id = $1
	`, sessionID, ids.runID).Scan(&sessionStatus, &currentRunID, &endedAt, &runExecutionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.SessionStatusCancelled || currentRunID != pgvalue.UUID(ids.runID) || !endedAt.Valid {
		t.Fatalf("session after cancel = status %s current %v ended %v, want cancelled/current-run/ended", sessionStatus, currentRunID, endedAt)
	}
	if runExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("run execution status = %s, want pending_cancel", runExecutionStatus)
	}
}

func TestCancelRunAllowsDisabledWorkerGroupForControlCancellation(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	seedCurrentAttempt(t, ctx, pool, ids)
	operation := seedCancelOperation(t, ctx, queries, ids, "wrong route")
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)

	_, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "wrong route",
		Force:         false,
		OperationID:   operation.ID,
	})
	if err != nil {
		t.Fatalf("CancelRun disabled worker group error = %v, want nil", err)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `
		SELECT status, execution_status
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&status, &executionStatus); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusCancelled || executionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("run state = status=%s execution_status=%s, want cancelled/finished", status, executionStatus)
	}
}

func TestDeadLetterRunQueueItemTerminalizesSession(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	seedCurrentAttempt(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DeadLetterRunQueueItem(ctx, db.DeadLetterRunQueueItemParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		QueueClass:    "default",
		LastError:     "dispatch retries exhausted",
	}); err != nil {
		t.Fatal(err)
	}

	var sessionStatus db.SessionStatus
	var currentRunID pgtype.UUID
	var endedAt pgtype.Timestamptz
	var runStatus db.RunStatus
	var terminalOutcome db.NullRunTerminalOutcome
	if err := pool.QueryRow(ctx, `
		SELECT sessions.status, sessions.current_run_id, session_runs.ended_at, runs.status, runs.terminal_outcome
		  FROM sessions
		  JOIN session_runs ON session_runs.org_id = sessions.org_id
		                   AND session_runs.session_id = sessions.id
		  JOIN runs ON runs.org_id = sessions.org_id
		           AND runs.id = $2
		 WHERE sessions.id = $1
	`, sessionID, ids.runID).Scan(&sessionStatus, &currentRunID, &endedAt, &runStatus, &terminalOutcome); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.SessionStatusOpen {
		t.Fatalf("session status = %s, want open", sessionStatus)
	}
	if currentRunID != pgvalue.UUID(ids.runID) {
		t.Fatalf("current_run_id = %v, want %v", currentRunID, ids.runID)
	}
	if !endedAt.Valid {
		t.Fatal("session_runs.ended_at was not set")
	}
	if runStatus != db.RunStatusFailed || !terminalOutcome.Valid || terminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeDeadLettered {
		t.Fatalf("run terminal state = %s/%v, want failed/dead_lettered", runStatus, terminalOutcome)
	}
}

func seedCurrentAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_attempt_number = 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
}

func seedSessionRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, sessionID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_runs (
			id, public_id, org_id, worker_group_id, project_id, environment_id, session_id, run_id, deployment_id, turn_index, reason
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, $7, $8, 1, 'initial')
	`, uuid.Must(uuid.NewV7()), ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, sessionID, ids.runID, ids.deploymentID, testSessionRunPublicID(t)); err != nil {
		t.Fatal(err)
	}
}

func seedCancelOperation(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, reason string) db.RunOperation {
	t.Helper()
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:       testRunOperationPublicID(t),
		OrgID:          pgvalue.UUID(ids.orgID),
		WorkerGroupID:  dbtest.DefaultWorkerGroupID,
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		RunID:          pgvalue.UUID(ids.runID),
		Kind:           db.RunOperationKindCancel,
		ActorKind:      "test",
		ActorID:        "test",
		Reason:         reason,
		Request:        []byte(`{}`),
		IdempotencyKey: "cancel:" + ids.runID.String() + ":" + reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}
