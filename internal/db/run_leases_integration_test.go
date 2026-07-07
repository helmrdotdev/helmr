package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLeaseRunLeaseRejectsConcurrentLeaseForSameQueueKey(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	firstWorkerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)
	secondWorkerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)

	firstRunID := ids.runID
	secondRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       queue_concurrency_limit = 1,
		       concurrency_key = 'shared-key',
		       current_run_lease_id = NULL,
		       workspace_mount_id = NULL,
		       dispatch_generation = 1,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, firstRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id,
			public_id,
			org_id,
			worker_group_id,
			project_id,
			environment_id,
			deployment_id,
			deployment_task_id,
			workspace_id,
			deployment_version,
			api_version,
			sdk_version,
			cli_version,
			task_id,
			session_id,
			status,
			execution_status,
			payload,
			metadata,
			tags,
			locked_retry_policy,
			queue_name,
			queue_concurrency_limit,
			concurrency_key,
			priority,
			queue_timestamp,
			ttl,
			queued_expires_at,
			requested_milli_cpu,
			requested_memory_mib,
			requested_disk_mib,
			requested_execution_slots,
			runtime_identity_id,
			runtime_arch,
			runtime_abi,
			kernel_digest,
			initramfs_digest,
			rootfs_digest,
			cni_profile,
			network_policy,
			placement,
			max_active_duration_ms,
			trace_id,
			root_span_id,
			current_attempt_number
		)
		SELECT $3,
		       $4,
		       org_id,
		       worker_group_id,
		       project_id,
		       environment_id,
		       deployment_id,
		       deployment_task_id,
		       workspace_id,
		       deployment_version,
		       api_version,
		       sdk_version,
		       cli_version,
		       task_id,
		       session_id,
		       'queued',
		       'queued',
		       payload,
		       metadata,
		       tags,
		       locked_retry_policy,
		       queue_name,
		       1,
		       'shared-key',
		       priority,
		       now(),
		       ttl,
		       NULL,
		       requested_milli_cpu,
		       requested_memory_mib,
		       requested_disk_mib,
		       requested_execution_slots,
		       runtime_identity_id,
		       runtime_arch,
		       runtime_abi,
		       kernel_digest,
		       initramfs_digest,
		       rootfs_digest,
		       cni_profile,
		       network_policy,
		       placement,
		       max_active_duration_ms,
		       trace_id,
		       root_span_id,
		       1
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, firstRunID, secondRunID, testPublicID(t, publicid.Run)); err != nil {
		t.Fatal(err)
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tx1.Rollback(context.Background())
	}()
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tx2.Rollback(context.Background())
	}()
	if _, err := lockAndLeaseRunLease(ctx, db.New(tx1), leaseRunLeaseParams(ids.orgID, firstRunID, firstWorkerID, "first")); err != nil {
		t.Fatal(err)
	}

	secondResult := make(chan error, 1)
	go func() {
		_, err := lockAndLeaseRunLease(ctx, db.New(tx2), leaseRunLeaseParams(ids.orgID, secondRunID, secondWorkerID, "second"))
		secondResult <- err
	}()

	select {
	case err := <-secondResult:
		if err == nil {
			t.Fatal("second lease succeeded before first transaction committed")
		}
		t.Fatalf("second lease returned before first transaction committed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second lease error = %v, want pgx.ErrNoRows", err)
	}
	if err := tx2.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	var activeLeases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM run_leases
		 WHERE org_id = $1
		   AND queue_name = 'default'
		   AND concurrency_key = 'shared-key'
		   AND status IN ('leased', 'running')
		   AND lease_expires_at > now()
	`, ids.orgID).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if activeLeases != 1 {
		t.Fatalf("active leases = %d, want 1", activeLeases)
	}
}

func lockAndLeaseRunLease(ctx context.Context, queries *db.Queries, params db.LeaseRunLeaseParams) (db.LeaseRunLeaseRow, error) {
	if err := queries.LockRunLeaseConcurrencyScope(ctx, db.LockRunLeaseConcurrencyScopeParams{
		OrgID: params.OrgID,
		RunID: params.RunID,
	}); err != nil {
		return db.LeaseRunLeaseRow{}, err
	}
	return queries.LeaseRunLease(ctx, params)
}

func TestLeaseRunLeaseRejectsStaleDispatchGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 2,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.LeaseRunLease(ctx, leaseRunLeaseParamsWithGeneration(ids.orgID, ids.runID, workerID, "stale", 1)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale lease error = %v, want pgx.ErrNoRows", err)
	}

	var leaseCount int
	var currentRunLeaseID uuid.NullUUID
	if err := pool.QueryRow(ctx, `
		SELECT count(run_leases.id)::int, runs.current_run_lease_id
		  FROM runs
		  LEFT JOIN run_leases
		    ON run_leases.org_id = runs.org_id
		   AND run_leases.run_id = runs.id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
		 GROUP BY runs.current_run_lease_id
	`, ids.orgID, ids.runID).Scan(&leaseCount, &currentRunLeaseID); err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 || currentRunLeaseID.Valid {
		t.Fatalf("leaseCount=%d currentRunLeaseID=%v, want no stale lease", leaseCount, currentRunLeaseID.Valid)
	}
}

func TestRequeueRunDispatchRejectsStaleDispatchGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 2,
		       dispatch_attempt_count = 0
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.RequeueRunDispatch(ctx, db.RequeueRunDispatchParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		WorkerGroupID:              dbtest.DefaultWorkerGroupID,
		QueueClass:                 "default",
		RunID:                      pgvalue.UUID(ids.runID),
		ExpectedDispatchGeneration: 1,
		LastError:                  "stale redis lease",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale requeue error = %v, want pgx.ErrNoRows", err)
	}

	var generation int64
	var attempts int32
	if err := pool.QueryRow(ctx, `
		SELECT dispatch_generation, dispatch_attempt_count
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&generation, &attempts); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || attempts != 0 {
		t.Fatalf("generation=%d attempts=%d, want generation 2 attempts 0", generation, attempts)
	}
}

func TestRequeueExpiredLeasedRunLeasesWritesLifecycleHistory(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)
	params := leaseRunLeaseParams(ids.orgID, ids.runID, workerID, "expired")

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 1,
		       dispatch_attempt_count = 0,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.LeaseRunLease(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, params.RunLeaseID); err != nil {
		t.Fatal(err)
	}

	if err := queries.RequeueExpiredLeasedRunLeases(ctx, db.RequeueExpiredLeasedRunLeasesParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}

	assertRunDispatchRequeuedLifecycle(t, ctx, pool, ids, params.RunLeaseID)
}

func TestAbandonLeasedRunLeaseWritesLifecycleHistory(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)
	params := leaseRunLeaseParams(ids.orgID, ids.runID, workerID, "abandoned")

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 1,
		       dispatch_attempt_count = 0,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.LeaseRunLease(ctx, params); err != nil {
		t.Fatal(err)
	}

	if err := queries.AbandonLeasedRunLease(ctx, db.AbandonLeasedRunLeaseParams{
		RunLeaseID:       params.RunLeaseID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
	}); err != nil {
		t.Fatal(err)
	}

	assertRunDispatchRequeuedLifecycle(t, ctx, pool, ids, params.RunLeaseID)
}

func TestFailExpiredRunningRunLeasesSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET locked_retry_policy = '{"enabled":true,"maxAttempts":3,"backoff":{"minMs":0,"maxMs":0,"factor":1,"jitter":"none"}}'::jsonb,
		       active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var terminalOutcome string
	var currentRunLeaseID uuid.NullUUID
	var currentAttempt int32
	var dispatchGeneration int64
	var leaseStatus db.RunLeaseStatus
	var sessionRunEndedAt pgtype.Timestamptz
	var outboxCount, usageCount int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       coalesce(runs.terminal_outcome::text, ''),
		       runs.current_run_lease_id,
		       runs.current_attempt_number,
		       runs.dispatch_generation,
		       run_leases.status,
		       session_runs.ended_at,
		       (SELECT count(*)::int FROM telemetry_outbox WHERE org_id = runs.org_id AND source_kind = 'run' AND source_id = runs.id AND stream_kind = 'event'),
		       (SELECT count(*)::int FROM meter_events WHERE org_id = runs.org_id AND run_id = runs.id AND meter = 'active_time')
		  FROM runs
		  JOIN run_leases ON run_leases.org_id = runs.org_id
		                 AND run_leases.run_id = runs.id
		                 AND run_leases.id = $3
		  JOIN session_runs ON session_runs.org_id = runs.org_id
		                   AND session_runs.project_id = runs.project_id
		                   AND session_runs.environment_id = runs.environment_id
		                   AND session_runs.session_id = runs.session_id
		                   AND session_runs.run_id = runs.id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID, runLeaseID).Scan(
		&status,
		&executionStatus,
		&terminalOutcome,
		&currentRunLeaseID,
		&currentAttempt,
		&dispatchGeneration,
		&leaseStatus,
		&sessionRunEndedAt,
		&outboxCount,
		&usageCount,
	); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued || executionStatus != db.RunExecutionStatusQueued || terminalOutcome != "" || currentRunLeaseID.Valid {
		t.Fatalf("run state = %s/%s terminal=%q currentLease=%v, want queued/queued/no terminal/no current lease", status, executionStatus, terminalOutcome, currentRunLeaseID.Valid)
	}
	if currentAttempt != 2 || dispatchGeneration != 2 {
		t.Fatalf("attempt=%d generation=%d, want 2/2", currentAttempt, dispatchGeneration)
	}
	if leaseStatus != db.RunLeaseStatusLost {
		t.Fatalf("run lease status = %s, want lost", leaseStatus)
	}
	if sessionRunEndedAt.Valid {
		t.Fatal("session run ended on retry, want open")
	}
	if outboxCount != 2 || usageCount != 1 {
		t.Fatalf("outbox=%d usage=%d, want 2/1", outboxCount, usageCount)
	}
	assertRunLifecycleTransitions(t, ctx, pool, ids, []string{"run.started", "run.failed", "run.retry_scheduled"}, []string{"run.failed", "run.retry_scheduled"})
}

func TestFailExpiredRunningRunLeasesTerminalizesWithoutRetry(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET locked_retry_policy = '{"enabled":false}'::jsonb,
		       active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var terminalOutcome string
	var currentRunLeaseID uuid.NullUUID
	var currentAttempt int32
	var leaseStatus db.RunLeaseStatus
	var sessionRunEndedAt pgtype.Timestamptz
	var outboxCount, usageCount int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       coalesce(runs.terminal_outcome::text, ''),
		       runs.current_run_lease_id,
		       runs.current_attempt_number,
		       run_leases.status,
		       session_runs.ended_at,
		       (SELECT count(*)::int FROM telemetry_outbox WHERE org_id = runs.org_id AND source_kind = 'run' AND source_id = runs.id AND stream_kind = 'event'),
		       (SELECT count(*)::int FROM meter_events WHERE org_id = runs.org_id AND run_id = runs.id AND meter = 'active_time')
		  FROM runs
		  JOIN run_leases ON run_leases.org_id = runs.org_id
		                 AND run_leases.run_id = runs.id
		                 AND run_leases.id = $3
		  JOIN session_runs ON session_runs.org_id = runs.org_id
		                   AND session_runs.project_id = runs.project_id
		                   AND session_runs.environment_id = runs.environment_id
		                   AND session_runs.session_id = runs.session_id
		                   AND session_runs.run_id = runs.id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID, runLeaseID).Scan(
		&status,
		&executionStatus,
		&terminalOutcome,
		&currentRunLeaseID,
		&currentAttempt,
		&leaseStatus,
		&sessionRunEndedAt,
		&outboxCount,
		&usageCount,
	); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusFailed || executionStatus != db.RunExecutionStatusFinished || terminalOutcome != "failed" || currentRunLeaseID.Valid {
		t.Fatalf("run state = %s/%s terminal=%q currentLease=%v, want failed/finished/failed/no current lease", status, executionStatus, terminalOutcome, currentRunLeaseID.Valid)
	}
	if currentAttempt != 1 {
		t.Fatalf("attempt=%d, want 1", currentAttempt)
	}
	if leaseStatus != db.RunLeaseStatusLost {
		t.Fatalf("run lease status = %s, want lost", leaseStatus)
	}
	if !sessionRunEndedAt.Valid {
		t.Fatal("session run ended_at was not set")
	}
	if outboxCount != 1 || usageCount != 1 {
		t.Fatalf("outbox=%d usage=%d, want 1/1", outboxCount, usageCount)
	}
	assertRunLifecycleTransitions(t, ctx, pool, ids, []string{"run.started", "run.failed"}, []string{"run.failed"})
}

func TestReleaseRunLeaseRetryFailsPendingCheckpointRestore(t *testing.T) {
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
		Request:         []byte(`{"reason":"retry_restore_failure_test"}`),
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
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET locked_retry_policy = '{"enabled":true,"maxAttempts":2,"backoff":{"minMs":0,"maxMs":0,"factor":1,"jitter":"none"}}'::jsonb
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	dispatchGeneration := currentRunDispatchGeneration(t, ctx, pool, ids.orgID, ids.runID)
	leased, err := queries.LeaseRunLease(ctx, leaseRunLeaseParamsWithGeneration(ids.orgID, ids.runID, workerID, "restore-retry-failure", dispatchGeneration))
	if err != nil {
		t.Fatal(err)
	}
	leasedRunLeaseID := pgvalue.MustUUIDValue(leased.RunLeaseID)
	if got := pgvalue.MustUUIDValue(leased.RunLeaseRestoreRunCheckpointID); got != checkpointed.runCheckpointID {
		t.Fatalf("restore run checkpoint id = %s, want %s", got, checkpointed.runCheckpointID)
	}
	assertRunCheckpointRestore(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, leasedRunLeaseID, workerID, db.RunCheckpointRestoreStatusRestoring)

	started, err := queries.StartRunLease(ctx, db.StartRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        leased.RunLeaseID,
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: leased.RunLeaseDispatchMessageID,
		DispatchLeaseID:   leased.RunLeaseDispatchLeaseID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != db.RunLeaseStatusRunning {
		t.Fatalf("started run lease status = %s, want running", started.Status)
	}

	released, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(ids.runID),
		RunLeaseID:           leased.RunLeaseID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		DispatchMessageID:    leased.RunLeaseDispatchMessageID,
		DispatchLeaseID:      leased.RunLeaseDispatchLeaseID,
		RunStatus:            db.RunStatusFailed,
		Output:               []byte(`{"ok":false}`),
		ErrorMessage:         pgtype.Text{String: "restore failed after resume", Valid: true},
		TerminalEventPayload: []byte(`{"failure_kind":"transient_error","detail":{"message":"restore failed after resume"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusQueued || released.ExecutionStatus != db.RunExecutionStatusQueued || released.CurrentAttemptNumber != 2 {
		t.Fatalf("released run state = %s/%s attempt=%d, want queued/queued/2", released.Status, released.ExecutionStatus, released.CurrentAttemptNumber)
	}
	assertRunCheckpointRestore(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, leasedRunLeaseID, workerID, db.RunCheckpointRestoreStatusFailed)
	assertRunLifecycleTransitions(t, ctx, pool, ids, []string{"run.started", "run.waiting", "run.resumed", "run_lease.leased", "run_lease.started", "run.failed", "run.retry_scheduled"}, []string{"run.waiting", "run.resumed", "run.failed", "run.retry_scheduled"})
}

func TestReleaseRunLeaseRetryBackoffPreventsImmediateLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET locked_retry_policy = '{"enabled":true,"maxAttempts":2,"backoff":{"minMs":3600000,"maxMs":3600000,"factor":1,"jitter":"none"}}'::jsonb,
		       active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(ids.runID),
		RunLeaseID:           pgvalue.UUID(runLeaseID),
		WorkerInstanceID:     pgvalue.UUID(workerID),
		DispatchMessageID:    "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:      "lease-" + runLeaseID.String()[:8],
		RunStatus:            db.RunStatusFailed,
		Output:               []byte(`{"ok":false}`),
		ErrorMessage:         pgtype.Text{String: "transient failure", Valid: true},
		TerminalEventPayload: []byte(`{"failure_kind":"transient_error","detail":{"message":"transient failure"}}`),
	}); err != nil {
		t.Fatal(err)
	}

	var dispatchGeneration int64
	var queueTimestamp time.Time
	if err := pool.QueryRow(ctx, `
		SELECT dispatch_generation, queue_timestamp
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&dispatchGeneration, &queueTimestamp); err != nil {
		t.Fatal(err)
	}
	if time.Until(queueTimestamp) < 30*time.Minute {
		t.Fatalf("queue timestamp = %s, want future retry backoff", queueTimestamp)
	}
	if _, err := queries.LeaseRunLease(ctx, leaseRunLeaseParamsWithGeneration(ids.orgID, ids.runID, workerID, "backoff", dispatchGeneration)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("immediate retry lease error = %v, want pgx.ErrNoRows", err)
	}
}

func TestRenewRunLeaseRejectsStaleDispatchGeneration(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, _ := seedExactCapacityRuntimeWorker(t, ctx, pool)
	params := leaseRunLeaseParams(ids.orgID, ids.runID, workerID, "renewal")

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 1,
		       queued_expires_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.LeaseRunLease(ctx, params); err != nil {
		t.Fatal(err)
	}
	renewParams := db.RenewRunLeaseParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: params.DispatchMessageID,
		DispatchLeaseID:   params.DispatchLeaseID,
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        params.RunLeaseID,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(2 * time.Hour)),
	}
	if _, err := queries.RenewRunLease(ctx, renewParams); err != nil {
		t.Fatalf("current renewal failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET dispatch_generation = 2
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	renewParams.LeaseExpiresAt = pgvalue.Timestamptz(time.Now().Add(3 * time.Hour))
	if _, err := queries.RenewRunLease(ctx, renewParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale renewal error = %v, want pgx.ErrNoRows", err)
	}
}

func TestLeaseRunLeaseRejectsExpiredRunCheckpointRestore(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWaitID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	runCheckpointID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       current_run_lease_id = NULL,
		       dispatch_generation = 1,
		       dispatch_attempt_count = 0,
		       queued_expires_at = NULL,
		       current_attempt_number = 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO waits (
			id, public_id, org_id, project_id, environment_id, kind, state, completed_after, completed_at
		)
		VALUES ($1, $5, $2, $3, $4, 'timer', 'completed', now() - interval '1 minute', now())
	`, waitID, ids.orgID, ids.projectID, ids.environmentID, testWaitPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, owner_run_id, base_version_id, acquired_version_id, acquired_fencing_generation,
			fencing_token, heartbeat_token, expires_at
		)
		SELECT $1, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.workspace_id, runs.workspace_mount_id,
		       'write', runs.id, workspaces.current_version_id, workspaces.current_version_id, 1,
		       'checkpoint-source-lease', 'checkpoint-source-heartbeat', now() + interval '1 hour'
		  FROM runs
		  JOIN workspaces ON workspaces.org_id = runs.org_id
		                 AND workspaces.project_id = runs.project_id
		                 AND workspaces.environment_id = runs.environment_id
		                 AND workspaces.id = runs.workspace_id
		 WHERE runs.org_id = $2
		   AND runs.id = $3
	`, workspaceLeaseID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_checkpoints (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
			state, runtime_backend, runtime_identity_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, owner_run_wait_id,
			owner_run_id, source_worker_instance_id, cni_profile, expires_at
		)
		SELECT $1, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.workspace_id, runs.id,
		       $6, runs.workspace_mount_id, workspaces.current_version_id,
		       'ready', 'firecracker', 'test-runtime', 'arm64', 'test', 'sha256:kernel',
		       'sha256:initramfs', 'sha256:rootfs', 'sha256:runtime-config', $4,
		       runs.id, $5, 'default', now() - interval '1 second'
		  FROM runs
		  JOIN workspaces ON workspaces.org_id = runs.org_id
		                 AND workspaces.project_id = runs.project_id
		                 AND workspaces.environment_id = runs.environment_id
		                 AND workspaces.id = runs.workspace_id
		 WHERE runs.org_id = $2
		   AND runs.id = $3
		 LIMIT 1
	`, runCheckpointID, ids.orgID, ids.runID, runWaitID, workerID, workspaceLeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET latest_run_checkpoint_id = $3
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID, runCheckpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_waits (
			id, org_id, worker_group_id, project_id, environment_id, run_id, wait_id,
			state, run_checkpoint_id, workspace_version_id, active_elapsed_ms_at_park
		)
		SELECT $1, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.id, $2,
		       'resuming', $3, workspaces.current_version_id, 0
		  FROM runs
		  JOIN workspaces ON workspaces.org_id = runs.org_id
		                 AND workspaces.project_id = runs.project_id
		                 AND workspaces.environment_id = runs.environment_id
		                 AND workspaces.id = runs.workspace_id
		 WHERE runs.org_id = $4
		   AND runs.id = $5
	`, runWaitID, waitID, runCheckpointID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunLease(ctx, leaseRunLeaseParams(ids.orgID, ids.runID, workerID, "expired-restore"))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired restore lease error = %v, want pgx.ErrNoRows", err)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var currentRunLeaseID uuid.NullUUID
	var runLeaseCount, workspaceLeaseCount, restoreCount int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       runs.current_run_lease_id,
		       (SELECT count(*)::int FROM run_leases WHERE org_id = runs.org_id AND run_id = runs.id AND dispatch_message_id = $3),
		       (SELECT count(*)::int FROM workspace_leases WHERE org_id = runs.org_id AND owner_run_id = runs.id AND fencing_token <> 'checkpoint-source-lease'),
		       (SELECT count(*)::int FROM run_checkpoint_restores WHERE org_id = runs.org_id AND run_id = runs.id)
		  FROM runs
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID, "dispatch-expired-restore").Scan(&status, &executionStatus, &currentRunLeaseID, &runLeaseCount, &workspaceLeaseCount, &restoreCount); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued || executionStatus != db.RunExecutionStatusQueued || currentRunLeaseID.Valid {
		t.Fatalf("expired restore run state = %s/%s currentLease=%v, want queued/queued/no current lease", status, executionStatus, currentRunLeaseID.Valid)
	}
	if runLeaseCount != 0 || workspaceLeaseCount != 0 || restoreCount != 0 {
		t.Fatalf("expired restore side effects = run leases %d workspace leases %d restores %d, want all 0", runLeaseCount, workspaceLeaseCount, restoreCount)
	}
}

func assertRunDispatchRequeuedLifecycle(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, runLeaseID pgtype.UUID) {
	t.Helper()
	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var currentRunLeaseID uuid.NullUUID
	var dispatchGeneration int64
	var dispatchAttempts int32
	var leaseStatus db.RunLeaseStatus
	var snapshotTransition, eventKind string
	var outboxCount int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       runs.current_run_lease_id,
		       runs.dispatch_generation,
		       runs.dispatch_attempt_count,
		       run_leases.status,
		       (SELECT transition FROM run_state_snapshots WHERE org_id = runs.org_id AND run_id = runs.id ORDER BY version DESC LIMIT 1),
			       (SELECT kind FROM telemetry_outbox WHERE org_id = runs.org_id AND run_id = runs.id AND stream_kind = 'event' ORDER BY id DESC LIMIT 1),
		       (SELECT count(*)::int FROM telemetry_outbox WHERE org_id = runs.org_id AND source_kind = 'run' AND source_id = runs.id AND stream_kind = 'event')
		  FROM runs
		  JOIN run_leases ON run_leases.org_id = runs.org_id
		                 AND run_leases.run_id = runs.id
		                 AND run_leases.id = $3
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID, runLeaseID).Scan(
		&status,
		&executionStatus,
		&currentRunLeaseID,
		&dispatchGeneration,
		&dispatchAttempts,
		&leaseStatus,
		&snapshotTransition,
		&eventKind,
		&outboxCount,
	); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued || executionStatus != db.RunExecutionStatusQueued || currentRunLeaseID.Valid {
		t.Fatalf("run state = %s/%s currentLease=%v, want queued/queued/no current lease", status, executionStatus, currentRunLeaseID.Valid)
	}
	if dispatchGeneration != 2 || dispatchAttempts != 1 {
		t.Fatalf("dispatch generation=%d attempts=%d, want 2/1", dispatchGeneration, dispatchAttempts)
	}
	if leaseStatus != db.RunLeaseStatusLost {
		t.Fatalf("run lease status = %s, want lost", leaseStatus)
	}
	if snapshotTransition != "run.dispatch_requeued" || eventKind != "run.dispatch_requeued" || outboxCount != 1 {
		t.Fatalf("requeue lifecycle = snapshot %q event %q outbox %d, want run.dispatch_requeued/run.dispatch_requeued/1", snapshotTransition, eventKind, outboxCount)
	}
}

func assertRunLifecycleTransitions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, wantSnapshots []string, wantEvents []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT transition
		  FROM run_state_snapshots
		 WHERE org_id = $1
		   AND run_id = $2
		 ORDER BY version ASC
	`, ids.orgID, ids.runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshots []string
	for rows.Next() {
		var transition string
		if err := rows.Scan(&transition); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, transition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != len(wantSnapshots) {
		t.Fatalf("snapshot transitions = %v, want %v", snapshots, wantSnapshots)
	}
	for i := range wantSnapshots {
		if snapshots[i] != wantSnapshots[i] {
			t.Fatalf("snapshot transitions = %v, want %v", snapshots, wantSnapshots)
		}
	}

	rows, err = pool.Query(ctx, `
		SELECT kind
			  FROM telemetry_outbox
			 WHERE org_id = $1
			   AND run_id = $2
			   AND stream_kind = 'event'
			 ORDER BY id ASC
	`, ids.orgID, ids.runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		events = append(events, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != len(wantEvents) {
		t.Fatalf("event kinds = %v, want %v", events, wantEvents)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("event kinds = %v, want %v", events, wantEvents)
		}
	}
}

func leaseRunLeaseParams(orgID, runID, workerID uuid.UUID, label string) db.LeaseRunLeaseParams {
	return leaseRunLeaseParamsWithGeneration(orgID, runID, workerID, label, 1)
}

func leaseRunLeaseParamsWithGeneration(orgID, runID, workerID uuid.UUID, label string, generation int64) db.LeaseRunLeaseParams {
	runLeaseID := uuid.Must(uuid.NewV7())
	return db.LeaseRunLeaseParams{
		WorkerInstanceID:   pgvalue.UUID(workerID),
		OrgID:              pgvalue.UUID(orgID),
		RunID:              pgvalue.UUID(runID),
		DispatchGeneration: generation,
		LeaseExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		RunLeaseID:         pgvalue.UUID(runLeaseID),
		DispatchMessageID:  "dispatch-" + label,
		DispatchLeaseID:    "lease-" + label,
		DispatchAttempt:    1,
		RunLeaseSpanID:     "3333333333333333",
	}
}
