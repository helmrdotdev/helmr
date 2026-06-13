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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRequeueExpiredLeasedRunExecutionSessionRestoresDispatchContract(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
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
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-expired-leased"),
		DispatchLeaseID:   "lease-expired-leased",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	queueItemBeforeRequeue := requireRunQueueItemDispatchState(t, ctx, pool, orgID, runID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := pool.Exec(ctx, `
	UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.RequeueExpiredLeasedRunExecutionSessions(ctx, orgID); err != nil {
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
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusLost)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	requireRunSessionEvent(t, ctx, pool, orgID, runID, sessionID, int32(1), "run.execution_lost", []byte(`{"reason":"worker lease expired before execution started","source":"lease_sweeper"}`))
	requireNoRunEventKind(t, ctx, pool, orgID, runID, "run.failed")

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "limited-expired-leased",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != runID || candidates[0].DispatchMessageID != "" {
		t.Fatalf("queue candidates = %+v", candidates)
	}
}

func TestMarkWaitpointCheckpointDurableReadyCompletesRestoredCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-next-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-next")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-restored-next"),
		DispatchLeaseID:   "lease-restored-next",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	restoreRunWaitID, restoreWaitpointID := requireWaitpointForCheckpoint(t, ctx, pool, orgID, runID, restoreCheckpointID)
	if _, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CheckpointID:     restoreCheckpointID,
		RunWaitID:        restoreRunWaitID,
		WaitpointID:      restoreWaitpointID,
	}); err != nil {
		t.Fatalf("second restore acknowledgement: %v", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, restoreWaitpointID, db.RunWaitStatusRestored)
	nextCheckpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	nextRunWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	nextWaitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:                  sessionID,
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
		WorkspaceArtifactDigest:    pgvalue.Text(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:        pgvalue.Text("copy-on-write"),
		ActiveDurationMs:           10000,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRuntimeConfigArtifact(t, ctx, pool, orgID, runID, nextCheckpointID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "active_time", 1, 10000)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "checkpoint_bytes", 1, 10)
	requireRunUsageDuration(t, ctx, pool, orgID, runID, 10000)
	requireRunUsageEventSnapshotTransition(t, ctx, pool, orgID, runID, "active_time", "checkpoint.ready")
	requireRunUsageEventSnapshotTransition(t, ctx, pool, orgID, runID, "checkpoint_bytes", "checkpoint.ready")

	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             nextWaitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgvalue.Text("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("admin"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: nextWaitpointID}); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-final")
	resumedSessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         resumedSessionID,
		DispatchMessageID: pgvalue.Text("message-restored-final"),
		DispatchLeaseID:   "lease-restored-final",
		DispatchAttempt:   2,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        resumedSessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET started_at = now() - interval '2 seconds'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, resumedSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                   orgID,
		RunID:                   runID,
		SessionID:               resumedSessionID,
		WorkerInstanceID:        instance.ID,
		DispatchMessageID:       "message-restored-final",
		DispatchLeaseID:         "lease-restored-final",
		RunStatus:               db.RunStatusFailed,
		AttemptStatus:           db.RunAttemptStatusFailed,
		ErrorMessage:            pgvalue.Text("worker failed after resume"),
		ReleaseActiveDurationMs: 0,
		TerminalEventKind:       "run.failed",
		TerminalEventPayload:    []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunUsageEventAtLeast(t, ctx, pool, orgID, runID, "active_time", 2, 11000)
}

func TestMarkWaitpointCheckpointDurableReadyRequiresLeaseRuntime(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-runtime")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-runtime")
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-checkpoint-runtime"),
		DispatchLeaseID:   "lease-checkpoint-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:                  sessionID,
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
		WorkspaceArtifactDigest:    pgvalue.Text(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:        pgvalue.Text("copy-on-write"),
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
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-backend")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-backend")
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-checkpoint-backend"),
		DispatchLeaseID:   "lease-checkpoint-backend",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:                  sessionID,
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
		WorkspaceArtifactDigest:    pgvalue.Text(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:        pgvalue.Text("copy-on-write"),
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
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-failed")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-checkpoint-failed"
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text(messageID),
		DispatchLeaseID:   "lease-checkpoint-failed",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		RunWaitID:        runWaitID,
		WaitpointID:      waitpointID,
		CheckpointID:     checkpointID,
		ErrorMessage:     pgvalue.Text("snapshot upload failed"),
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

func TestLeaseRunExecutionSessionRequiresRestoreRuntimeSnapshot(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
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

	_, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgvalue.Text("message-missing-runtime"),
		DispatchLeaseID:   "lease-missing-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
}

func TestRespondBeforeRunWaitUnblocksAfterCheckpointReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	waitpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateHumanWaitpoint(ctx, db.CreateHumanWaitpointParams{
		ID:                    waitpointID,
		OrgID:                 orgID,
		ProjectID:             scope.ProjectID,
		EnvironmentID:         scope.EnvironmentID,
		Request:               []byte(`{"message":"approve"}`),
		DisplayText:           "approve",
		ExpiresAt:             pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		IdempotencyKeyOptions: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          "api:owner",
		RequestHash:          "same-request",
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgvalue.Text("completed"),
		Resolution:           approvedWaitpointResolution("owner@example.com"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgvalue.Text("owner@example.com"),
		CompletedVia:         pgvalue.Text("authenticated_api"),
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
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-pre-respond"),
		DispatchLeaseID:   "lease-pre-respond",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
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
		SessionID:                  sessionID,
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
		WorkspaceArtifactDigest:    pgvalue.Text(testDigest("7")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:        pgvalue.Text("copy-on-write"),
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

func TestReleaseRestoredExecutionFailureInvalidatesRestoreCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := pgvalue.UUID(dbtest.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-failure")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-failure")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgvalue.Text("message-restored-failure"),
		DispatchLeaseID:   "lease-restored-failure",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-restored-failure",
		DispatchLeaseID:      "lease-restored-failure",
		RunStatus:            db.RunStatusFailed,
		AttemptStatus:        db.RunAttemptStatusFailed,
		ErrorMessage:         pgvalue.Text("restore failed"),
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusInvalid)
}
