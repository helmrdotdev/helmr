package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "wait-expired-opening",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		ID:               waitpointID,
		Kind:             db.WaitpointKindApproval,
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
	SELECT status, resolution_kind
	  FROM waitpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, waitpointID).Scan(&waitpointStatus, &resolutionKind); err != nil {
		t.Fatal(err)
	}
	if waitpointStatus != db.WaitpointStatusCancelled || resolutionKind.String != "cancelled" {
		t.Fatalf("waitpoint status = %s resolution = %+v", waitpointStatus, resolutionKind)
	}
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
		ID:               ids.ToPG(ids.New()),
		Kind:             db.WaitpointKindApproval,
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
	nextCheckpointID := ids.ToPG(ids.New())
	nextWaitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "next-waitpoint",
		CheckpointID:     nextCheckpointID,
		CheckpointReason: "waitpoint",
		ID:               nextWaitpointID,
		Kind:             db.WaitpointKindApproval,
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
		WaitpointID:                nextWaitpointID,
		CheckpointID:               nextCheckpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             pgText("firecracker"),
		RuntimeArch:                pgText("x86_64"),
		RuntimeABI:                 pgText("helmr.firecracker.snapshot.v1"),
		KernelDigest:               pgText("sha256:kernel"),
		RootfsDigest:               pgText("sha256:rootfs"),
		RuntimeConfigDigest:        pgText("sha256:runtime-config"),
		WorkspaceBaseKind:          pgText("github"),
		WorkspaceRepository:        pgText("helmrdotdev/helmr"),
		WorkspaceRef:               pgText("main"),
		WorkspaceSha:               pgText("0123456789abcdef0123456789abcdef01234567"),
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v1.tar"),
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
	    execution_id,
	    status,
	    reason,
	    runtime_backend,
	    runtime_arch,
	    runtime_abi,
	    kernel_digest,
	    rootfs_digest,
	    runtime_config_digest,
	    workspace_base_kind,
	    workspace_repository,
	    workspace_ref,
	    workspace_sha,
	    workspace_artifact_digest,
	    workspace_artifact_media_type,
	    workspace_artifact_encoding,
	    workspace_mount_path,
	    workspace_volume_kind,
	    manifest,
	    ready_at
	) VALUES (
	    $1,
	    $2,
	    $3,
	    $4,
	    'ready',
	    'waitpoint',
	    'firecracker',
	    'x86_64',
	    'helmr.firecracker.snapshot.v1',
	    'sha256:kernel',
	    'sha256:rootfs',
	    'sha256:runtime-config',
	    'github',
	    'helmrdotdev/helmr',
	    'main',
	    '0123456789abcdef0123456789abcdef01234567',
	    $5,
	    'application/vnd.helmr.workspace.v1.tar',
	    'tar',
	    '/workspace',
	    'copy-on-write',
	    '{"runtime":{"backend":"firecracker"}}',
	    now()
	)
	`, checkpointID, orgID, runID, executionID, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO waitpoints (
	    id,
	    org_id,
	    run_id,
	    execution_id,
	    checkpoint_id,
	    correlation_id,
	    kind,
	    request,
	    display_text,
	    status,
	    resolution_kind,
	    resolution,
	    requested_at,
	    resolved_at
	) VALUES ($1, $2, $3, $4, $5, 'restore-waitpoint', 'approval', '{"message":"approve"}', 'approve', 'resuming', 'approved', '{"approved":true}', now(), now())
	`, waitpointID, orgID, runID, executionID, checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_availability_replicas (
	    org_id,
	    run_id,
	    checkpoint_id,
	    state,
	    worker_instance_id,
	    execution_id,
	    dispatch_message_id,
	    dispatch_lease_id,
	    lease_expires_at,
	    metadata
	) VALUES ($1, $2, $3, 'durable', $4, $5, 'previous-message', 'previous-lease', now() + interval '1 minute', '{"source":"test"}')
	`, orgID, runID, checkpointID, workerInstanceID, executionID); err != nil {
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

func testCheckpointArtifactsJSON(t *testing.T) []byte {
	t.Helper()
	rows := []map[string]any{
		{"role": "runtime_manifest", "ordinal": 0, "digest": testDigest("1"), "size_bytes": 1, "media_type": cas.CheckpointManifestMediaType},
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
		RuntimeABI:              "helmr.firecracker.snapshot.v1",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v1",
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
