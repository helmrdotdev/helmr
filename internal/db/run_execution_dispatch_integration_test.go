package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLeaseRunExecutionBindsWorkerInstanceDispatchLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-a")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-a")

	executionID := ids.ToPG(ids.New())
	leased, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-a"),
		DispatchLeaseID:   "lease-a",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.ExecutionWorkerInstanceID != instance.ID {
		t.Fatalf("leased worker instance = %v, want %v", leased.ExecutionWorkerInstanceID, instance.ID)
	}
	if leased.ExecutionDispatchMessageID != "message-a" || leased.ExecutionDispatchLeaseID != "lease-a" || leased.ExecutionDispatchAttempt != 1 {
		t.Fatalf("leased redis lease fields = (%q, %q, %d)", leased.ExecutionDispatchMessageID, leased.ExecutionDispatchLeaseID, leased.ExecutionDispatchAttempt)
	}
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-a"),
		DispatchLeaseID:   "lease-b",
		DispatchAttempt:   2,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim error = %v, want no rows", err)
	}

	if status, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil || status != db.RunStatusRunning {
		t.Fatalf("start status = %q, err = %v", status, err)
	}
	if _, err := queries.RenewRunQueueReservation(ctx, db.RenewRunQueueReservationParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText("message-a"),
		ReservationExpiresAt: pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RenewRunExecutionLease(ctx, db.RenewRunExecutionLeaseParams{
		OrgID:             orgID,
		RunID:             runID,
		ExecutionID:       executionID,
		WorkerInstanceID:  instance.ID,
		DispatchMessageID: "message-a",
		DispatchLeaseID:   "lease-a",
		LeaseExpiresAt:    pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                orgID,
		RunID:                runID,
		ExecutionID:          executionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-a",
		DispatchLeaseID:      "lease-a",
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusSucceeded {
		t.Fatalf("released status = %q", released.Status)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
}

func TestLeaseRunExecutionRequiresMatchingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-worker-group-a")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-worker-group")
	secondWorkerGroupID := createPostgresTestWorkerGroup(t, ctx, pool, "lease-secondary")
	if _, err := pool.Exec(ctx, `
UPDATE run_runtime_requirements
   SET worker_group_id = $1
 WHERE org_id = $2
   AND run_id = $3
`, secondWorkerGroupID, orgID, runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-worker-group"),
		DispatchLeaseID:   "lease-worker-group",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
}

func TestLeaseRunExecutionSeparatesWorkerGroupsWithinSharedQueue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instanceA := upsertTestWorkerInstance(t, ctx, queries, "runner-shared-queue-a")
	workerGroupB := createPostgresTestWorkerGroup(t, ctx, pool, "lease-shared-queue-b")
	instanceB := upsertTestWorkerInstanceInGroup(t, ctx, queries, "runner-shared-queue-b", workerGroupB)
	runA := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	runB := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runA, "shared-queue", instanceA, "message-shared-a")
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runB, "shared-queue", instanceB, "message-shared-b")

	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runB,
		WorkerInstanceID:  instanceA.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-shared-b"),
		DispatchLeaseID:   "lease-shared-b",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-group lease error = %v, want no rows", err)
	}
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runA,
		WorkerInstanceID:  instanceA.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-shared-a"),
		DispatchLeaseID:   "lease-shared-a",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRunExecutionHonorsQueuedExpiry(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-queued")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `UPDATE runs SET queued_expires_at = now() - interval '1 second' WHERE org_id = $1 AND id = $2`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-expired")

	_, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-expired"),
		DispatchLeaseID:   "lease-expired",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
}

func TestLeaseRunExecutionHonorsQueueConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-limited-queue")
	firstRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	secondRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET queue_name = 'limited',
       queue_concurrency_limit = 1
 WHERE org_id = $1
   AND id IN ($2, $3)
`, orgID, firstRunID, secondRunID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, firstRunID, "limited", instance, "message-limited-a")
	seedLeasableRunQueueItem(t, ctx, queries, orgID, secondRunID, "limited", instance, "message-limited-b")

	type leaseAttempt struct {
		runID     pgtype.UUID
		execID    pgtype.UUID
		messageID string
		leaseID   string
	}
	type leaseResult struct {
		attempt leaseAttempt
		err     error
	}
	attempts := []leaseAttempt{{
		runID:     firstRunID,
		execID:    ids.ToPG(ids.New()),
		messageID: "message-limited-a",
		leaseID:   "lease-limited-a",
	}, {
		runID:     secondRunID,
		execID:    ids.ToPG(ids.New()),
		messageID: "message-limited-b",
		leaseID:   "lease-limited-b",
	}}
	start := make(chan struct{})
	results := make(chan leaseResult, len(attempts))
	var wg sync.WaitGroup
	for _, attempt := range attempts {
		wg.Go(func() {
			<-start
			_, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
				OrgID:             orgID,
				RunID:             attempt.runID,
				WorkerInstanceID:  instance.ID,
				ExecutionID:       attempt.execID,
				DispatchMessageID: pgText(attempt.messageID),
				DispatchLeaseID:   attempt.leaseID,
				DispatchAttempt:   1,
				LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
			})
			results <- leaseResult{attempt: attempt, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	var leased leaseAttempt
	var blocked leaseAttempt
	var leasedCount int
	var blockedCount int
	for result := range results {
		switch {
		case result.err == nil:
			leased = result.attempt
			leasedCount++
		case errors.Is(result.err, pgx.ErrNoRows):
			blocked = result.attempt
			blockedCount++
		default:
			t.Fatalf("lease error = %v", result.err)
		}
	}
	if leasedCount != 1 || blockedCount != 1 {
		t.Fatalf("leased=%d blocked=%d, want one lease and one blocked", leasedCount, blockedCount)
	}
	requireActiveConcurrencySlot(t, ctx, pool, orgID, leased.runID, leased.execID)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, blocked.runID, blocked.execID)

	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            leased.runID,
		ExecutionID:      leased.execID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                orgID,
		RunID:                leased.runID,
		ExecutionID:          leased.execID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    leased.messageID,
		DispatchLeaseID:      leased.leaseID,
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             blocked.runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText(blocked.messageID),
		DispatchLeaseID:   blocked.leaseID,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRequeueExpiredLeasedRunExecutionRestoresDispatchContract(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-leased")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	queuedExpiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET queue_name = 'limited-expired-leased',
	       queue_concurrency_limit = 1,
	       concurrency_key = 'same-key',
	       ttl = '1h',
	       queued_expires_at = $3
	 WHERE org_id = $1
	   AND id = $2
	`, orgID, runID, queuedExpiresAt); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "limited-expired-leased", instance, "message-expired-leased")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-expired-leased"),
		DispatchLeaseID:   "lease-expired-leased",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	queueItemBeforeRequeue := requireRunQueueItemDispatchState(t, ctx, pool, orgID, runID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := pool.Exec(ctx, `
	UPDATE run_executions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, executionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.RequeueExpiredLeasedRunExecutions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	runAfterRequeue, err := queries.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if !runAfterRequeue.QueuedExpiresAt.Valid || !runAfterRequeue.QueuedExpiresAt.Time.Equal(queuedExpiresAt) {
		t.Fatalf("run queued expiry = %+v, want %s", runAfterRequeue.QueuedExpiresAt, queuedExpiresAt)
	}
	queueItemAfterRequeue := requireRunQueueItemDispatchState(t, ctx, pool, orgID, runID)
	if queueItemAfterRequeue.Status != db.RunQueueStatusQueued {
		t.Fatalf("run queue status = %s, want %s", queueItemAfterRequeue.Status, db.RunQueueStatusQueued)
	}
	if !queueItemAfterRequeue.QueuedExpiresAt.Valid || !queueItemAfterRequeue.QueuedExpiresAt.Time.Equal(queuedExpiresAt) {
		t.Fatalf("run queue queued expiry = %+v, want %s", queueItemAfterRequeue.QueuedExpiresAt, queuedExpiresAt)
	}
	if queueItemAfterRequeue.DispatchGeneration != queueItemBeforeRequeue.DispatchGeneration+1 {
		t.Fatalf("dispatch generation = %d, want %d", queueItemAfterRequeue.DispatchGeneration, queueItemBeforeRequeue.DispatchGeneration+1)
	}
	if queueItemAfterRequeue.DispatchMessageID.Valid ||
		queueItemAfterRequeue.ReservedByWorkerInstanceID.Valid ||
		queueItemAfterRequeue.ReservationExpiresAt.Valid {
		t.Fatalf("queue reservation fields after requeue = %+v", queueItemAfterRequeue)
	}
	if queueItemAfterRequeue.LastError != "worker lease expired before execution started" {
		t.Fatalf("queue last error = %q", queueItemAfterRequeue.LastError)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireRunExecutionStatus(t, ctx, pool, orgID, runID, executionID, db.RunExecutionStatusLost)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, runID, executionID)
	requireRunExecutionEvent(t, ctx, pool, orgID, runID, executionID, int32(1), "run.execution_lost", []byte(`{"reason":"worker lease expired before execution started","source":"lease_sweeper"}`))
	requireNoRunEventKind(t, ctx, pool, orgID, runID, "run.failed")

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:     orgID,
		QueueName: "limited-expired-leased",
		RowLimit:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != runID || candidates[0].DispatchMessageID != "" {
		t.Fatalf("queue candidates = %+v", candidates)
	}
}

func TestFailExpiredRunningRunExecutionsSweepsOpeningWaitpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-opening")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-opening")
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-opening"),
		DispatchLeaseID:   "lease-opening",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "wait-expired-opening",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE run_executions
	   SET lease_expires_at = now() - interval '1 second'
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, executionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunExecutions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed || run.CurrentExecutionID.Valid || run.ErrorMessage.String != "worker lease expired" {
		t.Fatalf("run after sweep = %+v", run)
	}
	requireRunExecutionStatus(t, ctx, pool, orgID, runID, executionID, db.RunExecutionStatusLost)
	var waitpointStatus db.WaitpointStatus
	var resolutionKind pgtype.Text
	if err := pool.QueryRow(ctx, `
	SELECT waitpoints.status, waitpoints.resolution_kind
	  FROM waitpoints
	  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
	                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
	 WHERE waitpoints.org_id = $1
	   AND run_wait_dependencies.run_id = $2
	   AND waitpoints.id = $3
	`, orgID, runID, waitpointID).Scan(&waitpointStatus, &resolutionKind); err != nil {
		t.Fatal(err)
	}
	if waitpointStatus != db.WaitpointStatusCancelled || resolutionKind.String != "cancelled" {
		t.Fatalf("waitpoint status = %s resolution = %+v", waitpointStatus, resolutionKind)
	}
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"worker lease expired","source":"lease_sweeper"}`))
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
	SELECT status
	  FROM checkpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, checkpointID).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid {
		t.Fatalf("checkpoint status = %s", checkpointStatus)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
	requireRunExecutionEvent(t, ctx, pool, orgID, runID, executionID, int32(1), "run.execution_lost", []byte(`{"reason":"worker lease expired","source":"lease_sweeper"}`))
	requireRunExecutionEvent(t, ctx, pool, orgID, runID, executionID, int32(1), "run.failed", []byte(`{"failure_kind":"worker_lease_expired","detail":{"message":"worker lease expired"}}`))
}

func TestReleaseRunExecutionSeparatesCancelledWaitpointOutputAndResolution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-release-cancelled-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-release-cancelled-waitpoint"
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-release-cancelled-waitpoint",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "release-cancelled-waitpoint",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                orgID,
		RunID:                runID,
		ExecutionID:          executionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    messageID,
		DispatchLeaseID:      "lease-release-cancelled-waitpoint",
		Status:               db.RunStatusFailed,
		ErrorMessage:         pgText("worker failed"),
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"worker failed","source":"release"}`))
}

func TestCreateWaitpointForExecutionRequiresRunningExecution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-leased-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-leased-waitpoint")
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-leased-waitpoint"),
		DispatchLeaseID:   "lease-leased-waitpoint",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "wait-before-start",
		CheckpointID:     ids.ToPG(ids.New()),
		CheckpointReason: "waitpoint",
		RunWaitID:        ids.ToPG(ids.New()),
		ID:               ids.ToPG(ids.New()),
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("create waitpoint error = %v, want no rows", err)
	}
}

func TestMarkWaitpointCheckpointDurableReadyCompletesRestoredCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-next-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-next")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-restored-next"),
		DispatchLeaseID:   "lease-restored-next",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	restoreRunWaitID, restoreWaitpointID := requireWaitpointForCheckpoint(t, ctx, pool, orgID, runID, restoreCheckpointID)
	if _, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CheckpointID:     restoreCheckpointID,
		RunWaitID:        restoreRunWaitID,
		WaitpointID:      restoreWaitpointID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CheckpointID:     restoreCheckpointID,
		RunWaitID:        restoreRunWaitID,
		WaitpointID:      restoreWaitpointID,
	}); err != nil {
		t.Fatalf("second restore acknowledgement: %v", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, restoreWaitpointID, db.RunWaitStatusRestored)
	nextCheckpointID := ids.ToPG(ids.New())
	nextRunWaitID := ids.ToPG(ids.New())
	nextWaitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "next-waitpoint",
		CheckpointID:     nextCheckpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        nextRunWaitID,
		ID:               nextWaitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE run_queue_items
	   SET reservation_expires_at = now() - interval '1 second'
	 WHERE org_id = $1
	   AND run_id = $2
	   AND reserved_by_worker_instance_id = $3
	   AND dispatch_message_id = 'message-restored-next'
	`, orgID, runID, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		ExecutionID:                executionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  nextRunWaitID,
		WaitpointID:                nextWaitpointID,
		CheckpointID:               nextCheckpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRuntimeConfigArtifact(t, ctx, pool, orgID, runID, nextCheckpointID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
}

func TestMarkWaitpointCheckpointDurableReadyRequiresLeaseRuntime(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-runtime")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-runtime")
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-checkpoint-runtime"),
		DispatchLeaseID:   "lease-checkpoint-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-runtime",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		ExecutionID:                executionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:other-kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"mismatch"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("checkpoint ready runtime mismatch error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusCreating)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusOpening)
	requireNoCheckpointArtifacts(t, ctx, pool, orgID, runID, checkpointID)
}

func TestMarkWaitpointCheckpointDurableReadyRejectsUnsupportedRuntimeBackend(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-backend")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-backend")
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-checkpoint-backend"),
		DispatchLeaseID:   "lease-checkpoint-backend",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-backend",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		ExecutionID:                executionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"test"}}`),
		RuntimeBackend:             "test",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"unsupported-backend"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("checkpoint ready backend error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusCreating)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusOpening)
	requireNoCheckpointArtifacts(t, ctx, pool, orgID, runID, checkpointID)
}

func TestMarkWaitpointCheckpointFailedSeparatesOutputAndResolution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-failed")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-checkpoint-failed"
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-checkpoint-failed",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-failed",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.MarkWaitpointCheckpointFailed(ctx, db.MarkWaitpointCheckpointFailedParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		RunWaitID:        runWaitID,
		WaitpointID:      waitpointID,
		CheckpointID:     checkpointID,
		ErrorMessage:     pgText("snapshot upload failed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != db.RunWaitStatusFailed || resolved.ResolutionKind.String != "cancelled" {
		t.Fatalf("resolved waitpoint = %+v", resolved)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusInvalid)
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"snapshot upload failed","source":"checkpoint"}`))
}

func TestLeaseRunExecutionRequiresRestoreRuntimeSnapshot(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restore-missing-runtime")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-missing-runtime")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	if _, err := pool.Exec(ctx, `
	DELETE FROM checkpoint_runtime_snapshots
	 WHERE org_id = $1
	   AND run_id = $2
	   AND checkpoint_id = $3
	`, orgID, runID, restoreCheckpointID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-missing-runtime"),
		DispatchLeaseID:   "lease-missing-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
}

func TestRespondWaitpointResponseTokenResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "token-single-response")
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, project_id, environment_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, waitpoints.project_id, waitpoints.environment_id, $4, '\x01', now() + interval '5 minutes', 'reviewer@example.com', '{}'
  FROM run_wait_dependencies
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                orgID,
		ID:                   tokenID,
		TokenHash:            []byte{1},
		CompletedByPrincipal: pgText("reviewer@example.com"),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		WaitpointID:    waitpointID,
		ResponseKey:    "email:reviewer@example.com",
		RequestHash:    "same",
		Action:         "respond",
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
		EventPayload:   []byte(`{"resolution_kind":"completed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireWaitpointCompletionPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"approved":true}`), approvedWaitpointResolution("reviewer@example.com"))
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestRespondBeforeRunWaitUnblocksAfterCheckpointReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateHumanWaitpoint(ctx, db.CreateHumanWaitpointParams{
		ID:                    waitpointID,
		OrgID:                 orgID,
		ProjectID:             scope.ProjectID,
		EnvironmentID:         scope.EnvironmentID,
		Request:               []byte(`{"message":"approve"}`),
		DisplayText:           "approve",
		ExpiresAt:             pgTime(time.Now().Add(time.Hour)),
		IdempotencyKeyOptions: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          "api:owner",
		RequestHash:          "same-request",
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("owner@example.com"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("owner@example.com"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, pgtype.UUID{}, waitpointID)); err != nil {
		t.Fatal(err)
	}
	if resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil || len(resumed) != 0 {
		t.Fatalf("pre-wait unblock = %+v, err = %v", resumed, err)
	}

	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-pre-respond")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-pre-respond")
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-pre-respond"),
		DispatchLeaseID:   "lease-pre-respond",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "pre-respond",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		ExecutionID:                executionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("7")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"pre-respond"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)

	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].RunWaitID != runWaitID || resumed[0].Status != db.RunWaitStatusResuming {
		t.Fatalf("resumed = %+v", resumed)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
}

func TestResolveWaitpointRecordsAndResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-single-response")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("admin"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindHuman,
		RequestHash:          "same",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("admin"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestResolveWaitpointRequiresSuspendedQueueEntryBeforeMutating(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-missing-suspended")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgText("completed"),
		Resolution:           []byte(`{"value":{"approved":true}}`),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindHuman,
		RequestHash:          "same",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_queue_items
   SET status = 'queued'
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
		t.Fatal(err)
	}
	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed = %d, want 0", len(resumed))
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireWaitpointConditionStatus(t, ctx, pool, orgID, waitpointID, db.WaitpointStatusCompleted)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusWaiting)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireNoRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestExpireDuePendingWaitpointsMarksConditionExpired(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "timeout-expired")
	if _, err := pool.Exec(ctx, `
UPDATE run_waits
   SET timeout_seconds = 1,
       waiting_at = now() - interval '2 seconds'
  FROM run_wait_dependencies
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.org_id = run_waits.org_id
   AND run_wait_dependencies.run_wait_id = run_waits.id
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID); err != nil {
		t.Fatal(err)
	}

	if err := queries.ExpireDuePendingWaitpoints(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireWaitpointConditionStatus(t, ctx, pool, orgID, waitpointID, db.WaitpointStatusExpired)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestConcurrentWaitpointTokenResponsesResolveOnce(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "token-concurrent-single")
	tokenID1 := seedWaitpointResponseToken(t, ctx, pool, orgID, runID, waitpointID, []byte{1}, "first@example.com")
	tokenID2 := seedWaitpointResponseToken(t, ctx, pool, orgID, runID, waitpointID, []byte{2}, "second@example.com")

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	q1 := db.New(tx1)
	if err := respondWaitpointToken(ctx, q1, orgID, waitpointID, tokenID1, []byte{1}, "token:first"); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		q2 := db.New(tx2)
		if err := respondWaitpointToken(ctx, q2, orgID, waitpointID, tokenID2, []byte{2}, "token:second"); err != nil {
			errCh <- err
			return
		}
		errCh <- tx2.Commit(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestMarkWaitpointDeliverySentWinsSameAttemptStaleRequeue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "delivery-stale-sent")
	deliveryID := ids.ToPG(ids.New())
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := queries.CreateQueuedWaitpointEmailDelivery(ctx, db.CreateQueuedWaitpointEmailDeliveryParams{
		DeliveryID:       deliveryID,
		OrgID:            orgID,
		RunID:            runID,
		WaitpointID:      waitpointID,
		TokenHash:        []byte{1},
		ExpiresAt:        pgTime(future),
		Recipient:        "owner@example.test",
		TokenMetadata:    []byte(`{}`),
		MessageID:        pgText("<waitpoint-delivery@example.test>"),
		DeliveryMetadata: []byte(`{"source":"test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWaitpointDeliveryForSend(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueStaleSendingWaitpointDeliveries(ctx, db.RequeueStaleSendingWaitpointDeliveriesParams{
		StaleBefore: pgTime(future),
		MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	sent, err := queries.MarkWaitpointDeliverySent(ctx, db.MarkWaitpointDeliverySentParams{
		OrgID:            orgID,
		DeliveryID:       deliveryID,
		AttemptCount:     claimed.AttemptCount,
		SendingStartedAt: claimed.SendingStartedAt,
		LastAttemptAt:    claimed.LastAttemptAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != db.WaitpointDeliveryStatusSent {
		t.Fatalf("delivery status = %s, want sent", sent.Status)
	}
}

func TestReleaseRestoredExecutionFailureInvalidatesRestoreCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-failure")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-failure")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText("message-restored-failure"),
		DispatchLeaseID:   "lease-restored-failure",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                orgID,
		RunID:                runID,
		ExecutionID:          executionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-restored-failure",
		DispatchLeaseID:      "lease-restored-failure",
		Status:               db.RunStatusFailed,
		ErrorMessage:         pgText("restore failed"),
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusInvalid)
}

func TestLostRunExecutionsExhaustDispatchAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-lost-attempts")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	for attempt := int32(1); attempt <= 2; attempt++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO run_executions (
    id,
    org_id,
    run_id,
    worker_instance_id,
    worker_group_id,
    dispatch_message_id,
    dispatch_lease_id,
    dispatch_attempt,
    status,
    lease_expires_at,
    runtime_id,
    worker_runtime_id,
    lost_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'lost', now() - interval '1 minute', $9, $9, now())
`, ids.ToPG(ids.New()), orgID, runID, instance.ID, instance.WorkerGroupID, "message-lost", "lease-lost", attempt, instance.RuntimeID); err != nil {
			t.Fatal(err)
		}
	}
	exhausted, err := queries.RunExecutionDispatchAttemptsExhausted(ctx, db.RunExecutionDispatchAttemptsExhaustedParams{
		OrgID:               orgID,
		RunID:               runID,
		MaxDispatchAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted {
		t.Fatal("dispatch attempts were not exhausted")
	}
}

func TestDeadLetterRunQueueItemFailsQueuedRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "dead-letter-runner")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "dead-letter-queue", instance, "dead-letter-message")

	deadLettered, err := queries.DeadLetterRunQueueItem(ctx, db.DeadLetterRunQueueItemParams{
		OrgID:             orgID,
		RunID:             runID,
		DispatchMessageID: pgText("dead-letter-message"),
		LastError:         "delivery exhausted",
		EventKind:         "run.dead_lettered",
		EventPayload:      []byte(`{"reason":"max_dispatch_attempts_exceeded"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deadLettered.Status != db.RunQueueStatusDeadLettered {
		t.Fatalf("dead letter status = %s", deadLettered.Status)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusDeadLettered)
}

func seedReadyRestoreCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, workerInstanceID pgtype.UUID) pgtype.UUID {
	t.Helper()
	executionID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO run_executions (
	    id,
	    org_id,
	    run_id,
	    worker_instance_id,
	    worker_group_id,
	    dispatch_message_id,
	    dispatch_lease_id,
	    dispatch_attempt,
	    status,
	    lease_expires_at,
	    runtime_id,
	    worker_runtime_id,
	    active_duration_ms,
	    released_at
	) VALUES ($1, $2, $3, $4, (SELECT worker_group_id FROM worker_instances WHERE id = $4), 'previous-message', 'previous-lease', 1, 'detached', now() + interval '1 minute', 'sha256:runtime', 'sha256:runtime', 100, now())
	`, executionID, orgID, runID, workerInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoints (
	    id,
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    execution_id,
	    status,
	    reason,
	    manifest,
	    ready_at
	)
	SELECT $1::uuid,
	       runs.org_id,
	       runs.id,
	       runs.project_id,
	       runs.environment_id,
	       $4,
	       'ready',
	       'waitpoint',
	       '{"runtime":{"backend":"firecracker"}}',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
	`, checkpointID, orgID, runID, executionID); err != nil {
		t.Fatal(err)
	}
	runtimeConfigArtifactID := ids.ToPG(ids.New())
	workspaceArtifactID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO cas_objects (digest, size_bytes, media_type)
	VALUES
	    ('sha256:runtime-config', 1, 'application/vnd.helmr.checkpoint.runtime-config.v0+json'),
	    ($1, 1, 'application/vnd.helmr.workspace.v0.tar')
	ON CONFLICT (digest) DO NOTHING
	`, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
	SELECT $1,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       'sha256:runtime-config',
	       'checkpoint_runtime_config'::artifact_kind,
	       1,
	       'application/vnd.helmr.checkpoint.runtime-config.v0+json'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	UNION ALL
	SELECT $2::uuid,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       $5,
	       'checkpoint_workspace'::artifact_kind,
	       1,
	       'application/vnd.helmr.workspace.v0.tar'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	`, runtimeConfigArtifactID, workspaceArtifactID, orgID, runID, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_runtime_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    runtime_backend,
	    runtime_id,
	    runtime_arch,
	    runtime_abi,
	    kernel_digest,
	    initramfs_digest,
	    rootfs_digest,
	    cni_profile,
	    runtime_config_artifact_id
	)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       'firecracker',
	       'sha256:runtime',
	       'x86_64',
	       'helmr.firecracker.snapshot.v0',
	       'sha256:kernel',
	       'sha256:initramfs',
	       'sha256:rootfs',
	       'helmr/v0',
	       $4
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
	`, orgID, runID, checkpointID, runtimeConfigArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_workspace_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    workspace_artifact_id,
		    workspace_artifact_encoding,
		    workspace_mount_path,
		    workspace_volume_kind
		)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       $4,
	       'tar',
	       '/workspace',
	       'copy-on-write'
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
		`, orgID, runID, checkpointID, workspaceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	WITH run_scope AS (
	    SELECT org_id, id AS run_id, project_id, environment_id
	      FROM runs
	     WHERE org_id = $2
	       AND id = $3
	),
	waitpoint AS (
	    INSERT INTO waitpoints (
	        id,
	        org_id,
	        project_id,
	        environment_id,
	        kind,
	        request,
	        display_text,
	        status,
	        resolution_kind,
	        output,
	        completed_at
	    )
	    SELECT $1,
	           run_scope.org_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           'human',
	           '{}',
	           'approve',
	           'completed',
	           'completed',
	           '{"value":{"approved":true}}',
	           now()
	      FROM run_scope
	    RETURNING *
	),
	run_wait AS (
	    INSERT INTO run_waits (
	        id,
	        org_id,
	        run_id,
	        project_id,
	        environment_id,
	        execution_id,
	        checkpoint_id,
	        correlation_id,
	        status,
	        resolution_kind,
	        resolution,
	        waiting_at,
	        resolved_at
	    )
	    SELECT $6,
	           run_scope.org_id,
	           run_scope.run_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           $4,
	           $5,
	           'restore-waitpoint',
	           'resuming',
	           'completed',
	           '{"value":{"approved":true}}',
	           now(),
	           now()
	      FROM waitpoint
	      JOIN run_scope ON true
	    RETURNING *
	)
	INSERT INTO run_wait_dependencies (
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    run_wait_id,
	    waitpoint_id
	)
	SELECT run_wait.org_id,
	       run_wait.run_id,
	       run_wait.project_id,
	       run_wait.environment_id,
	       run_wait.id,
	       waitpoint.id
	  FROM run_wait
	  JOIN waitpoint ON true
	`, waitpointID, orgID, runID, executionID, checkpointID, runWaitID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET latest_checkpoint_id = $1
	 WHERE org_id = $2
	   AND id = $3
	`, checkpointID, orgID, runID); err != nil {
		t.Fatal(err)
	}
	return checkpointID
}

func requireCheckpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID, want db.CheckpointStatus) {
	t.Helper()
	var got db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
	SELECT status
	  FROM checkpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, checkpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("checkpoint status = %s, want %s", got, want)
	}
}

func requireRuntimeConfigArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var artifactID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT runtime_config_artifact_id
  FROM checkpoint_runtime_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	if !artifactID.Valid {
		t.Fatal("runtime_config_artifact_id is null")
	}
}

func requireNoCheckpointArtifacts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM checkpoint_artifacts
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("checkpoint artifact rows = %d, want 0", count)
	}
}

func requireWaitpointForCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	var runWaitID pgtype.UUID
	var waitpointID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT run_waits.id, run_wait_dependencies.waitpoint_id
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_waits.checkpoint_id = $3
	`, orgID, runID, checkpointID).Scan(&runWaitID, &waitpointID); err != nil {
		t.Fatal(err)
	}
	return runWaitID, waitpointID
}

func requireWaitpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want db.RunWaitStatus) {
	t.Helper()
	var got db.RunWaitStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint status = %s, want %s", got, want)
	}
}

func requireWaitpointConditionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, waitpointID pgtype.UUID, want db.WaitpointStatus) {
	t.Helper()
	var got db.WaitpointStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM waitpoints
 WHERE org_id = $1
   AND id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint condition status = %s, want %s", got, want)
	}
}

func requireRunStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want db.RunStatus) {
	t.Helper()
	var got db.RunStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run status = %s, want %s", got, want)
	}
}

func requireRunExecutionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, executionID pgtype.UUID, want db.RunExecutionStatus) {
	t.Helper()
	var got db.RunExecutionStatus
	var lostAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
SELECT status, lost_at
  FROM run_executions
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, executionID).Scan(&got, &lostAt); err != nil {
		t.Fatal(err)
	}
	if got != want || (want == db.RunExecutionStatusLost && !lostAt.Valid) {
		t.Fatalf("run execution status = %s lost_at = %+v, want %s", got, lostAt, want)
	}
}

func requireNoActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, executionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND execution_id = $3
   AND released_at IS NULL
`, orgID, runID, executionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active concurrency slots = %d, want 0", count)
	}
}

func requireActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, executionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND execution_id = $3
   AND released_at IS NULL
`, orgID, runID, executionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active concurrency slots = %d, want 1", count)
	}
}

func requireWaitpointResponseCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM waitpoint_responses
 WHERE org_id = $1
   AND waitpoint_id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint response count = %d, want %d", got, want)
	}
}

func requireWaitpointCompletionPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantOutput, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, responseResolution []byte
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       run_waits.resolution,
       waitpoint_responses.resolution
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
  JOIN waitpoint_responses ON waitpoint_responses.org_id = waitpoints.org_id
                          AND waitpoint_responses.waitpoint_id = waitpoints.id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &runWaitResolution, &responseResolution); err != nil {
		t.Fatal(err)
	}
	requireCanonicalJSON(t, "waitpoint output", output, wantOutput)
	requireCanonicalJSON(t, "waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "waitpoint response resolution", responseResolution, wantResolution)
}

func requireCancelledWaitpointPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, runWaitFailure []byte
	var outputIsError bool
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       waitpoints.output_is_error,
       run_waits.resolution,
       run_waits.failure
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &outputIsError, &runWaitResolution, &runWaitFailure); err != nil {
		t.Fatal(err)
	}
	if !outputIsError {
		t.Fatal("waitpoint output_is_error = false, want true")
	}
	requireCanonicalJSON(t, "cancelled waitpoint output", output, []byte(`null`))
	requireCanonicalJSON(t, "cancelled waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait failure", runWaitFailure, wantResolution)
}

func requireCanonicalJSON(t *testing.T, name string, got []byte, want []byte) {
	t.Helper()
	gotCanonical := canonicalJSON(t, got)
	wantCanonical := canonicalJSON(t, want)
	if gotCanonical != wantCanonical {
		t.Fatalf("%s = %s, want %s", name, gotCanonical, wantCanonical)
	}
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", string(raw), err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}

func requireRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("run event %q not found", kind)
	}
}

func requireRunExecutionEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, executionID pgtype.UUID, attemptNumber int32, kind string, wantPayload []byte) {
	t.Helper()
	var gotExecutionID pgtype.UUID
	var gotAttemptNumber pgtype.Int4
	var gotPayload []byte
	if err := pool.QueryRow(ctx, `
SELECT execution_id, attempt_number, payload
  FROM run_events
 WHERE org_id = $1
   AND run_id = $2
   AND execution_id = $3
   AND kind = $4
 ORDER BY id DESC
 LIMIT 1
`, orgID, runID, executionID, kind).Scan(&gotExecutionID, &gotAttemptNumber, &gotPayload); err != nil {
		t.Fatal(err)
	}
	if gotExecutionID != executionID || !gotAttemptNumber.Valid || gotAttemptNumber.Int32 != attemptNumber {
		t.Fatalf("run event %q execution = %+v attempt = %+v, want execution %s attempt %d", kind, gotExecutionID, gotAttemptNumber, ids.MustFromPG(executionID), attemptNumber)
	}
	requireCanonicalJSON(t, "run event payload", gotPayload, wantPayload)
}

func requireNoRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("run event %q count = %d, want 0", kind, count)
	}
}

func seedWaitingWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, orgID pgtype.UUID, suffix string) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-"+suffix)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-" + suffix
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		ExecutionID:       executionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-" + suffix,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    suffix,
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		ExecutionID:                executionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusSuspended)
	return runID, waitpointID
}

func seedWaitpointResponseToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, tokenHash []byte, externalSubject string) pgtype.UUID {
	t.Helper()
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, project_id, environment_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, waitpoints.project_id, waitpoints.environment_id, $4, $5, now() + interval '5 minutes', $6, '{}'
  FROM run_wait_dependencies
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID, tokenHash, externalSubject); err != nil {
		t.Fatal(err)
	}
	return tokenID
}

func respondWaitpointToken(ctx context.Context, queries *db.Queries, orgID, waitpointID, tokenID pgtype.UUID, tokenHash []byte, responseKey string) error {
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                orgID,
		ID:                   tokenID,
		TokenHash:            tokenHash,
		CompletedByPrincipal: pgText(responseKey),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          responseKey,
		RequestHash:          responseKey,
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution(responseKey),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText(responseKey),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		return err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, pgtype.UUID{}, waitpointID)); err != nil {
		return err
	}
	_, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	return err
}

func resolveApprovedWaitpointParams(orgID, runID, waitpointID pgtype.UUID) db.ResolveWaitpointParams {
	return db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
	}
}

func approvedWaitpointResolution(principal string) []byte {
	payload, err := json.Marshal(map[string]any{
		"value":     map[string]any{"approved": true},
		"principal": principal,
		"at":        "2026-04-23T00:00:00Z",
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func testCheckpointArtifactsJSON(t *testing.T) []byte {
	t.Helper()
	rows := []map[string]any{
		{"role": "runtime_config", "ordinal": 0, "digest": testDigest("1"), "size_bytes": 1, "media_type": cas.CheckpointRuntimeConfigMediaType},
		{"role": "runtime_vmstate", "ordinal": 0, "digest": testDigest("2"), "size_bytes": 2, "media_type": cas.CheckpointVMStateMediaType},
		{"role": "runtime_memory", "ordinal": 0, "digest": testDigest("3"), "size_bytes": 3, "media_type": cas.CheckpointMemoryMediaType},
		{"role": "runtime_scratch_disk", "ordinal": 0, "digest": testDigest("4"), "size_bytes": 4, "media_type": cas.CheckpointScratchDiskMediaType},
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testDigest(char string) string {
	return "sha256:" + strings.Repeat(char, 64)
}

func seedLeasableRunQueueItem(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, queueName string, instance db.UpsertWorkerInstanceHeartbeatRow, messageID string) {
	t.Helper()
	if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeID:               instance.RuntimeID,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v0",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
		WorkerGroupID:           instance.WorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := queries.UpsertRunQueueItemQueued(ctx, db.UpsertRunQueueItemQueuedParams{
		RunID:             runID,
		OrgID:             orgID,
		Priority:          10,
		QueueName:         queueName,
		QueueTimestamp:    pgTime(time.Now()),
		DispatchMessageID: pgText(messageID),
	})
	if err != nil {
		t.Fatal(err)
	}
	publishTestRunQueueItem(t, ctx, queries, orgID, runID, entry, messageID)
	if _, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText(messageID),
		ReservationExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

type runQueueItemDispatchState struct {
	Status                     db.RunQueueStatus
	QueuedExpiresAt            pgtype.Timestamptz
	DispatchMessageID          pgtype.Text
	ReservedByWorkerInstanceID pgtype.UUID
	ReservationExpiresAt       pgtype.Timestamptz
	DispatchGeneration         int64
	LastError                  string
}

func requireRunQueueItemDispatchState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID) runQueueItemDispatchState {
	t.Helper()
	var got runQueueItemDispatchState
	if err := pool.QueryRow(ctx, `
SELECT status,
       queued_expires_at,
       dispatch_message_id,
       reserved_by_worker_instance_id,
       reservation_expires_at,
       dispatch_generation,
       last_error
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(
		&got.Status,
		&got.QueuedExpiresAt,
		&got.DispatchMessageID,
		&got.ReservedByWorkerInstanceID,
		&got.ReservationExpiresAt,
		&got.DispatchGeneration,
		&got.LastError,
	); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireRunQueueItemStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID, want db.RunQueueStatus) {
	t.Helper()
	var got db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run queue status = %s, want %s", got, want)
	}
}
