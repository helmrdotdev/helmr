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
		attempt := attempt
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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
		Kind:             db.WaitpointKindToken,
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
	var executionStatus db.RunExecutionStatus
	var lostAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
	SELECT status, lost_at
	  FROM run_executions
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, executionID).Scan(&executionStatus, &lostAt); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusLost || !lostAt.Valid {
		t.Fatalf("execution status = %s lost_at = %+v", executionStatus, lostAt)
	}
	var waitpointStatus db.WaitpointStatus
	var resolutionKind pgtype.Text
	if err := pool.QueryRow(ctx, `
	SELECT waitpoints.status, waitpoints.completion_kind
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
		Kind:             db.WaitpointKindToken,
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
		Kind:             db.WaitpointKindToken,
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
		Kind:             db.WaitpointKindToken,
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
		RuntimeBackend:             pgText("firecracker"),
		RuntimeArch:                pgText("x86_64"),
		RuntimeABI:                 pgText("helmr.firecracker.snapshot.v0"),
		KernelDigest:               pgText("sha256:kernel"),
		RootfsDigest:               pgText("sha256:rootfs"),
		RuntimeConfigDigest:        pgText("sha256:runtime-config"),
		WorkspaceBaseKind:          pgText("github"),
		WorkspaceRepository:        pgText("helmrdotdev/helmr"),
		WorkspaceRef:               pgText("main"),
		WorkspaceSha:               pgText("0123456789abcdef0123456789abcdef01234567"),
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
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
		Kind:             db.WaitpointKindToken,
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

func TestCompleteWaitpointResponseTokenResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "token-single-response")
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, run_id, run_wait_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, $3, run_wait_dependencies.run_wait_id, $4, '\x01', now() + interval '5 minutes', 'reviewer@example.com', '{}'
  FROM run_wait_dependencies
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteWaitpointResponseToken(ctx, db.CompleteWaitpointResponseTokenParams{
		ID:                   tokenID,
		TokenHash:            []byte{1},
		Action:               "complete",
		Kind:                 db.WaitpointKindToken,
		CompletedByPrincipal: pgText("reviewer@example.com"),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
		ResponseID:           ids.ToPG(ids.New()),
		ResponseKey:          "email:reviewer@example.com",
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("reviewer@example.com"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireWaitpointCompletionPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"approved":true}`), approvedWaitpointResolution("reviewer@example.com"))
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestResolveWaitpointRecordsAndResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-single-response")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "complete",
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("admin"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		RunID:                runID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindToken,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          orgID,
		RunID:          runID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindToken,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("admin"),
		Payload:        []byte(`{"resolution_kind":"completed"}`),
	}); err != nil {
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
		Action:               "complete",
		ResolutionKind:       pgText("completed"),
		Resolution:           []byte(`{"value":{"approved":true}}`),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		RunID:                runID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindToken,
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

	_, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resolve error = %v, want ErrNoRows", err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireWaitpointConditionStatus(t, ctx, pool, orgID, waitpointID, db.WaitpointStatusPending)
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
	if _, err := q1.CompleteWaitpointResponseToken(ctx, completeWaitpointTokenParams(tokenID1, []byte{1}, "token:first")); err != nil {
		t.Fatal(err)
	}
	if _, err := q1.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
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
		if _, err := q2.CompleteWaitpointResponseToken(ctx, completeWaitpointTokenParams(tokenID2, []byte{2}, "token:second")); err != nil {
			errCh <- err
			return
		}
		_, _ = q2.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID))
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
    dispatch_message_id,
    dispatch_lease_id,
    dispatch_attempt,
    status,
    lease_expires_at,
    lost_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'lost', now() - interval '1 minute', now())
`, ids.ToPG(ids.New()), orgID, runID, instance.ID, "message-lost", "lease-lost", attempt); err != nil {
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
	    dispatch_message_id,
	    dispatch_lease_id,
	    dispatch_attempt,
	    status,
	    lease_expires_at,
	    active_duration_ms,
	    released_at
	) VALUES ($1, $2, $3, $4, 'previous-message', 'previous-lease', 1, 'detached', now() + interval '1 minute', 100, now())
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
	SELECT $1,
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
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_runtime_snapshots (
	    org_id,
	    run_id,
	    checkpoint_id,
	    runtime_backend,
	    runtime_arch,
	    runtime_abi,
	    kernel_digest,
	    rootfs_digest,
	    runtime_config_digest
	) VALUES ($1, $2, $3, 'firecracker', 'x86_64', 'helmr.firecracker.snapshot.v0', 'sha256:kernel', 'sha256:rootfs', 'sha256:runtime-config')
	`, orgID, runID, checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_workspace_snapshots (
	    org_id,
	    run_id,
	    checkpoint_id,
	    workspace_base_kind,
	    workspace_repository,
	    workspace_ref,
	    workspace_sha,
	    workspace_artifact_digest,
	    workspace_artifact_media_type,
	    workspace_artifact_encoding,
	    workspace_mount_path,
	    workspace_volume_kind
	) VALUES ($1, $2, $3, 'github', 'helmrdotdev/helmr', 'main', '0123456789abcdef0123456789abcdef01234567', $4, 'application/vnd.helmr.workspace.v0.tar', 'tar', '/workspace', 'copy-on-write')
	`, orgID, runID, checkpointID, testDigest("6")); err != nil {
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
	        completion_kind,
	        output,
	        completed_at
	    )
	    SELECT $1,
	           run_scope.org_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           'token',
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

func requireWaitpointResponseCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM waitpoint_responses
 WHERE org_id = $1
   AND run_id = $2
   AND waitpoint_id = $3
`, orgID, runID, waitpointID).Scan(&got); err != nil {
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
                          AND waitpoint_responses.run_id = run_waits.run_id
                          AND waitpoint_responses.run_wait_id = run_waits.id
                          AND waitpoint_responses.waitpoint_id = waitpoints.id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &runWaitResolution, &responseResolution); err != nil {
		t.Fatal(err)
	}
	requireCanonicalJSON(t, "waitpoint output", output, wantOutput)
	requireCanonicalJSON(t, "waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "run wait resolution", runWaitResolution, wantOutput)
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
		Kind:             db.WaitpointKindToken,
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
		RuntimeBackend:             pgText("firecracker"),
		RuntimeArch:                pgText("x86_64"),
		RuntimeABI:                 pgText("helmr.firecracker.snapshot.v0"),
		KernelDigest:               pgText("sha256:kernel"),
		RootfsDigest:               pgText("sha256:rootfs"),
		RuntimeConfigDigest:        pgText("sha256:runtime-config"),
		WorkspaceBaseKind:          pgText("github"),
		WorkspaceRepository:        pgText("helmrdotdev/helmr"),
		WorkspaceRef:               pgText("main"),
		WorkspaceSha:               pgText("0123456789abcdef0123456789abcdef01234567"),
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
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
INSERT INTO waitpoint_response_tokens (id, org_id, run_id, run_wait_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, $3, run_wait_dependencies.run_wait_id, $4, $5, now() + interval '5 minutes', $6, '{}'
  FROM run_wait_dependencies
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID, tokenHash, externalSubject); err != nil {
		t.Fatal(err)
	}
	return tokenID
}

func completeWaitpointTokenParams(tokenID pgtype.UUID, tokenHash []byte, responseKey string) db.CompleteWaitpointResponseTokenParams {
	return db.CompleteWaitpointResponseTokenParams{
		ID:                   tokenID,
		TokenHash:            tokenHash,
		Action:               "complete",
		Kind:                 db.WaitpointKindToken,
		CompletedByPrincipal: pgText(responseKey),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
		ResponseID:           ids.ToPG(ids.New()),
		ResponseKey:          responseKey,
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution(responseKey),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
	}
}

func resolveApprovedWaitpointParams(orgID, runID, waitpointID pgtype.UUID) db.ResolveWaitpointParams {
	return db.ResolveWaitpointParams{
		OrgID:          orgID,
		RunID:          runID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindToken,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
		Payload:        []byte(`{"resolution_kind":"completed"}`),
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

func seedLeasableRunQueueItem(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, queueName string, instance db.WorkerInstance, messageID string) {
	t.Helper()
	if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v0",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
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
