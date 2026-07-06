package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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

func TestForceCancelRunCleansRuntimeAuthority(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	checkpointed := createCheckpointedRunWait(t, ctx, queries, ids, runLeaseID, workerID)

	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		ID:     pgvalue.UUID(checkpointed.runWaitID),
		Result: []byte(`{"timer":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 {
		t.Fatalf("requeued waits = %d, want 1", len(requeued))
	}
	mount, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RequestPriority: requeued[0].Priority,
		Request:         []byte(`{"reason":"force_cancel_cleanup_test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetQueuedRunWorkspaceMount(ctx, db.SetQueuedRunWorkspaceMountParams{
		WorkspaceMountID: mount.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		WorkspaceID:      pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}

	dispatchGeneration := currentRunDispatchGeneration(t, ctx, pool, ids.orgID, ids.runID)
	leased, err := queries.LeaseRunLease(ctx, leaseRunLeaseParamsWithGeneration(ids.orgID, ids.runID, workerID, "force-cancel-restore", dispatchGeneration))
	if err != nil {
		t.Fatal(err)
	}
	leasedRunLeaseID := pgvalue.MustUUIDValue(leased.RunLeaseID)
	if _, err := queries.StartRunLease(ctx, db.StartRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        leased.RunLeaseID,
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: leased.RunLeaseDispatchMessageID,
		DispatchLeaseID:   leased.RunLeaseDispatchLeaseID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	operation := seedCancelOperation(t, ctx, queries, ids, "force cleanup")

	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "force cleanup",
		Force:         true,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var currentRunLeaseID uuid.NullUUID
	var runLeaseStatus db.RunLeaseStatus
	var activeWorkspaceLeases, releasedWorkspaceLeases, usageCount int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       runs.current_run_lease_id,
		       run_leases.status,
		       (SELECT count(*)::int FROM workspace_leases WHERE org_id = runs.org_id AND owner_run_id = runs.id AND state = 'active'),
		       (SELECT count(*)::int FROM workspace_leases WHERE org_id = runs.org_id AND owner_run_id = runs.id AND state = 'released'),
		       (SELECT count(*)::int FROM usage_ledger_entries WHERE org_id = runs.org_id AND run_id = runs.id AND meter = 'active_time')
		  FROM runs
		  JOIN run_leases ON run_leases.org_id = runs.org_id
		                 AND run_leases.run_id = runs.id
		                 AND run_leases.id = $3
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID, leasedRunLeaseID).Scan(
		&status,
		&executionStatus,
		&currentRunLeaseID,
		&runLeaseStatus,
		&activeWorkspaceLeases,
		&releasedWorkspaceLeases,
		&usageCount,
	); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusCancelled || executionStatus != db.RunExecutionStatusFinished || currentRunLeaseID.Valid {
		t.Fatalf("run state = %s/%s currentLease=%v, want cancelled/finished/no current lease", status, executionStatus, currentRunLeaseID.Valid)
	}
	if runLeaseStatus != db.RunLeaseStatusCancelled {
		t.Fatalf("run lease status = %s, want cancelled", runLeaseStatus)
	}
	if activeWorkspaceLeases != 0 || releasedWorkspaceLeases == 0 {
		t.Fatalf("workspace leases active=%d released=%d, want none active and at least one released", activeWorkspaceLeases, releasedWorkspaceLeases)
	}
	if usageCount != 1 {
		t.Fatalf("active_time usage count = %d, want 1", usageCount)
	}
	assertRuntimeCheckpointRestore(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, leasedRunLeaseID, workerID, db.RuntimeCheckpointRestoreStatusFailed)
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

func TestDeadLetterRunDispatchTerminalizesSession(t *testing.T) {
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
	if _, err := queries.DeadLetterRunDispatch(ctx, db.DeadLetterRunDispatchParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WorkerGroupID:      dbtest.DefaultWorkerGroupID,
		RunID:              pgvalue.UUID(ids.runID),
		QueueClass:         "default",
		DispatchGeneration: 1,
		LastError:          "dispatch retries exhausted",
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

	var snapshotTransition, eventKind string
	var outboxCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT transition FROM run_state_snapshots WHERE org_id = $1 AND run_id = $2 ORDER BY version DESC LIMIT 1),
			(SELECT kind FROM event_hot_payloads WHERE org_id = $1 AND run_id = $2 ORDER BY seq DESC LIMIT 1),
			(SELECT count(*)::int FROM telemetry_outbox WHERE org_id = $1 AND source_kind = 'run' AND source_id = $2 AND stream_kind = 'event')
	`, ids.orgID, ids.runID).Scan(&snapshotTransition, &eventKind, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if snapshotTransition != "run.dead_lettered" || eventKind != "run.dead_lettered" || outboxCount != 1 {
		t.Fatalf("dead-letter lifecycle = snapshot %q event %q outbox %d, want run.dead_lettered/run.dead_lettered/1", snapshotTransition, eventKind, outboxCount)
	}
}

func TestDeadLetterRunDispatchRejectsStaleDispatchGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       dispatch_generation = 2
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.DeadLetterRunDispatch(ctx, db.DeadLetterRunDispatchParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WorkerGroupID:      dbtest.DefaultWorkerGroupID,
		RunID:              pgvalue.UUID(ids.runID),
		QueueClass:         "default",
		DispatchGeneration: 1,
		LastError:          "stale redis message",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("dead-letter stale generation error = %v, want pgx.ErrNoRows", err)
	}

	var status db.RunStatus
	var generation int64
	if err := pool.QueryRow(ctx, `
		SELECT status, dispatch_generation
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&status, &generation); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued || generation != 2 {
		t.Fatalf("run status=%s generation=%d, want queued generation 2", status, generation)
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
