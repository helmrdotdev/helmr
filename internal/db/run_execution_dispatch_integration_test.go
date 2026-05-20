package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLeaseRunExecutionBindsWorkerHostDispatchLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "exec-group", "exec-queue")
	host := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "runner-a")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           group.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v1",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerGroupID:  group.ID,
		Priority:       10,
		QueueName:      group.QueueName,
		QueueMessageID: "message-a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryLeased(ctx, db.MarkRunQueueEntryLeasedParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  group.ID,
		WorkerHostID:   host.ID,
		QueueMessageID: "message-a",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	executionID := ids.ToPG(ids.New())
	leased, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:           orgID,
		RunID:           runID,
		WorkerGroupID:   group.ID,
		WorkerHostID:    host.ID,
		ExecutionID:     executionID,
		QueueMessageID:  "message-a",
		QueueLeaseID:    "lease-a",
		DeliveryAttempt: 1,
		LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.ExecutionWorkerGroupID != group.ID || leased.ExecutionWorkerHostID != host.ID {
		t.Fatalf("leased worker identity = (%v, %v), want (%v, %v)", leased.ExecutionWorkerGroupID, leased.ExecutionWorkerHostID, group.ID, host.ID)
	}
	if leased.ExecutionQueueMessageID != "message-a" || leased.ExecutionQueueLeaseID != "lease-a" || leased.ExecutionDeliveryAttempt != 1 {
		t.Fatalf("leased redis lease fields = (%q, %q, %d)", leased.ExecutionQueueMessageID, leased.ExecutionQueueLeaseID, leased.ExecutionDeliveryAttempt)
	}
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:           orgID,
		RunID:           runID,
		WorkerGroupID:   group.ID,
		WorkerHostID:    host.ID,
		ExecutionID:     ids.ToPG(ids.New()),
		QueueMessageID:  "message-a",
		QueueLeaseID:    "lease-b",
		DeliveryAttempt: 2,
		LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim error = %v, want no rows", err)
	}

	if status, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:         orgID,
		RunID:         runID,
		ExecutionID:   executionID,
		WorkerGroupID: group.ID,
		WorkerHostID:  host.ID,
	}); err != nil || status != db.RunStatusRunning {
		t.Fatalf("start status = %q, err = %v", status, err)
	}
	if _, err := queries.RenewRunQueueLease(ctx, db.RenewRunQueueLeaseParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  group.ID,
		WorkerHostID:   host.ID,
		QueueMessageID: "message-a",
		LeaseExpiresAt: pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RenewRunExecutionLease(ctx, db.RenewRunExecutionLeaseParams{
		OrgID:          orgID,
		RunID:          runID,
		ExecutionID:    executionID,
		WorkerGroupID:  group.ID,
		WorkerHostID:   host.ID,
		QueueMessageID: "message-a",
		QueueLeaseID:   "lease-a",
		LeaseExpiresAt: pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                orgID,
		RunID:                runID,
		ExecutionID:          executionID,
		WorkerGroupID:        group.ID,
		WorkerHostID:         host.ID,
		QueueMessageID:       "message-a",
		QueueLeaseID:         "lease-a",
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
	requireRunQueueEntryStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
}

func TestResolvedWaitpointContinuationRequeuesCompletedDispatch(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	fixture := seedCompletedWaitpointCheckpoint(t, ctx, queries, pool, pgtype.Int4{})

	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: "approved", Valid: true},
		Resolution:     []byte(`{"approved":true}`),
		OrgID:          fixture.orgID,
		RunID:          fixture.runID,
		ID:             fixture.waitpointID,
		Kind:           db.WaitpointKindApproval,
		Payload:        []byte(`{"resolution_kind":"approved"}`),
	}); err != nil {
		t.Fatal(err)
	}

	requireWaitpointContinuationDispatchable(t, ctx, queries, fixture.orgID, fixture.runID)
}

func TestCompletedWaitpointResponseTokenContinuationRequeuesCompletedDispatch(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	fixture := seedCompletedWaitpointCheckpoint(t, ctx, queries, pool, pgtype.Int4{})
	tokenID := ids.ToPG(ids.New())
	tokenHash := []byte("waitpoint-response-token-hash")

	if _, err := queries.CreateWaitpointResponseToken(ctx, db.CreateWaitpointResponseTokenParams{
		ID:             tokenID,
		TokenHash:      tokenHash,
		AllowedActions: []string{"approve"},
		Metadata:       []byte(`{}`),
		OrgID:          fixture.orgID,
		RunID:          fixture.runID,
		WaitpointID:    fixture.waitpointID,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := queries.CompleteWaitpointResponseToken(ctx, db.CompleteWaitpointResponseTokenParams{
		ID:                   tokenID,
		TokenHash:            tokenHash,
		Action:               "approve",
		Kind:                 db.WaitpointKindApproval,
		ResolutionKind:       pgtype.Text{String: "approved", Valid: true},
		Resolution:           []byte(`{"approved":true}`),
		Payload:              []byte(`{"resolution_kind":"approved"}`),
		CompletedByPrincipal: pgtype.Text{String: "test-user", Valid: true},
		CompletedVia:         pgtype.Text{String: "waitpoint_response_token", Valid: true},
		Metadata:             []byte(`{"completed":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.WaitpointResponseTokenStatusCompleted {
		t.Fatalf("token status = %q, want completed", completed.Status)
	}

	requireWaitpointContinuationDispatchable(t, ctx, queries, fixture.orgID, fixture.runID)
}

func TestExpiredWaitpointContinuationRequeuesCompletedDispatch(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		expire func(context.Context, *db.Queries, waitpointDispatchFixture) error
	}{
		{
			name: "sweeper",
			expire: func(ctx context.Context, queries *db.Queries, fixture waitpointDispatchFixture) error {
				return queries.ExpireDuePendingWaitpoints(ctx, fixture.orgID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries, pool := newPostgresTestDB(t, ctx)
			fixture := seedCompletedWaitpointCheckpoint(t, ctx, queries, pool, pgtype.Int4{Int32: 1, Valid: true})
			if _, err := pool.Exec(ctx, `
UPDATE waitpoints
   SET requested_at = now() - interval '2 seconds'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, fixture.orgID, fixture.runID, fixture.waitpointID); err != nil {
				t.Fatal(err)
			}

			if err := tt.expire(ctx, queries, fixture); err != nil {
				t.Fatal(err)
			}

			requireWaitpointContinuationDispatchable(t, ctx, queries, fixture.orgID, fixture.runID)
		})
	}
}

func TestSuccessfulWaitpointContinuationsDoNotExhaustDeliveryAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	fixture := seedCompletedWaitpointCheckpoint(t, ctx, queries, pool, pgtype.Int4{})

	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: "approved", Valid: true},
		Resolution:     []byte(`{"approved":true}`),
		OrgID:          fixture.orgID,
		RunID:          fixture.runID,
		ID:             fixture.waitpointID,
		Kind:           db.WaitpointKindApproval,
		Payload:        []byte(`{"resolution_kind":"approved"}`),
	}); err != nil {
		t.Fatal(err)
	}
	prepared := requireWaitpointContinuationDispatchable(t, ctx, queries, fixture.orgID, fixture.runID)
	if _, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                fixture.orgID,
		RunID:                fixture.runID,
		QueueMessageID:       "message-continuation",
		ExpectedQueueVersion: prepared.QueueVersion,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryLeased(ctx, db.MarkRunQueueEntryLeasedParams{
		OrgID:          fixture.orgID,
		RunID:          fixture.runID,
		WorkerGroupID:  fixture.workerGroupID,
		WorkerHostID:   fixture.workerHostID,
		QueueMessageID: "message-continuation",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	continuationExecutionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:           fixture.orgID,
		RunID:           fixture.runID,
		WorkerGroupID:   fixture.workerGroupID,
		WorkerHostID:    fixture.workerHostID,
		ExecutionID:     continuationExecutionID,
		QueueMessageID:  "message-continuation",
		QueueLeaseID:    "lease-continuation",
		DeliveryAttempt: 2,
		LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:         fixture.orgID,
		RunID:         fixture.runID,
		ExecutionID:   continuationExecutionID,
		WorkerGroupID: fixture.workerGroupID,
		WorkerHostID:  fixture.workerHostID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                fixture.orgID,
		RunID:                fixture.runID,
		ExecutionID:          continuationExecutionID,
		WorkerGroupID:        fixture.workerGroupID,
		WorkerHostID:         fixture.workerHostID,
		QueueMessageID:       "message-continuation",
		QueueLeaseID:         "lease-continuation",
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		Output:               []byte(`{"ok":true}`),
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunQueueEntryStatus(t, ctx, pool, fixture.orgID, fixture.runID, db.RunQueueStatusCompleted)

	exhausted, err := queries.RunExecutionDeliveryAttemptsExhausted(ctx, db.RunExecutionDeliveryAttemptsExhaustedParams{
		OrgID:               fixture.orgID,
		RunID:               fixture.runID,
		WorkerGroupID:       fixture.workerGroupID,
		MaxDeliveryAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted {
		t.Fatal("successful detached/released waitpoint continuation executions exhausted delivery attempts")
	}
}

func TestLostRunExecutionsExhaustDeliveryAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "lost-attempts", "lost-attempts-queue")
	host := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "runner-lost-attempts")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	for attempt := int32(1); attempt <= 2; attempt++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO run_executions (
    id,
    org_id,
    run_id,
    worker_group_id,
    worker_host_id,
    queue_message_id,
    queue_lease_id,
    delivery_attempt,
    status,
    lease_expires_at,
    lost_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'lost', now() - interval '1 minute', now())
`, ids.ToPG(ids.New()), orgID, runID, group.ID, host.ID, "lost-message-"+string(rune('0'+attempt)), "lost-lease-"+string(rune('0'+attempt)), attempt); err != nil {
			t.Fatal(err)
		}
	}

	exhausted, err := queries.RunExecutionDeliveryAttemptsExhausted(ctx, db.RunExecutionDeliveryAttemptsExhaustedParams{
		OrgID:               orgID,
		RunID:               runID,
		WorkerGroupID:       group.ID,
		MaxDeliveryAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted {
		t.Fatal("lost delivery attempts did not exhaust")
	}
}

func requireRunQueueEntryStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID, want db.RunQueueStatus) {
	t.Helper()
	var status db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_queue_entries
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("dispatch status = %q, want %q", status, want)
	}
}

type waitpointDispatchFixture struct {
	orgID          pgtype.UUID
	runID          pgtype.UUID
	workerGroupID  pgtype.UUID
	workerHostID   pgtype.UUID
	executionID    pgtype.UUID
	waitpointID    pgtype.UUID
	checkpointID   pgtype.UUID
	queueMessageID string
}

func seedCompletedWaitpointCheckpoint(t *testing.T, ctx context.Context, queries *db.Queries, pool *pgxpool.Pool, timeoutSeconds pgtype.Int4) waitpointDispatchFixture {
	t.Helper()
	orgID := ids.ToPG(ids.DefaultOrgID)

	if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	suffix := ids.New().String()
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "waitpoint-"+suffix, "waitpoint-queue-"+suffix)
	host := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "runner-waitpoint-"+suffix)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           group.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v1",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	queueMessageID := "message-waitpoint-" + suffix
	if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerGroupID:  group.ID,
		Priority:       10,
		QueueName:      group.QueueName,
		QueueMessageID: queueMessageID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryLeased(ctx, db.MarkRunQueueEntryLeasedParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  group.ID,
		WorkerHostID:   host.ID,
		QueueMessageID: queueMessageID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:           orgID,
		RunID:           runID,
		WorkerGroupID:   group.ID,
		WorkerHostID:    host.ID,
		ExecutionID:     executionID,
		QueueMessageID:  queueMessageID,
		QueueLeaseID:    "lease-waitpoint-" + suffix,
		DeliveryAttempt: 1,
		LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:         orgID,
		RunID:         runID,
		ExecutionID:   executionID,
		WorkerGroupID: group.ID,
		WorkerHostID:  host.ID,
	}); err != nil {
		t.Fatal(err)
	}

	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		ExecutionID:      executionID,
		WorkerGroupID:    group.ID,
		WorkerHostID:     host.ID,
		CorrelationID:    "correlation-" + suffix,
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		ID:               waitpointID,
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"prompt":"approve?"}`),
		DisplayText:      "Approve?",
		TimeoutSeconds:   timeoutSeconds,
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := queries.MarkWaitpointCheckpointReady(ctx, db.MarkWaitpointCheckpointReadyParams{
		OrgID:             orgID,
		RunID:             runID,
		ExecutionID:       executionID,
		WorkerGroupID:     group.ID,
		WorkerHostID:      host.ID,
		WaitpointID:       waitpointID,
		CheckpointID:      checkpointID,
		CasObjects:        []byte(`[]`),
		Manifest:          []byte(`{}`),
		RuntimeBackend:    pgtype.Text{String: "firecracker", Valid: true},
		RuntimeArch:       pgtype.Text{String: "x86_64", Valid: true},
		RuntimeABI:        pgtype.Text{String: "helmr.firecracker.snapshot.v0", Valid: true},
		KernelDigest:      pgtype.Text{String: "sha256:kernel", Valid: true},
		RootfsDigest:      pgtype.Text{String: "sha256:rootfs", Valid: true},
		RuntimeVcpus:      pgtype.Int4{Int32: 4, Valid: true},
		RuntimeMemoryMib:  pgtype.Int4{Int32: 8192, Valid: true},
		CniProfile:        pgtype.Text{String: "helmr/v1", Valid: true},
		MemoryDigests:     []byte(`[]`),
		ActiveDurationMs:  123,
		CheckpointPayload: []byte(`{"checkpoint":"ready"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != db.WaitpointStatusPending {
		t.Fatalf("waitpoint status = %q, want pending", ready.Status)
	}
	var runStatus db.RunStatus
	var currentExecutionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT status, current_execution_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&runStatus, &currentExecutionID); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusWaiting || currentExecutionID.Valid {
		t.Fatalf("run status/current_execution = %q/%v, want waiting/null", runStatus, currentExecutionID)
	}
	var queueStatus db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_queue_entries
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(&queueStatus); err != nil {
		t.Fatal(err)
	}
	if queueStatus != db.RunQueueStatusCompleted {
		t.Fatalf("queue status = %q, want completed", queueStatus)
	}

	return waitpointDispatchFixture{
		orgID:          orgID,
		runID:          runID,
		workerGroupID:  group.ID,
		workerHostID:   host.ID,
		executionID:    executionID,
		waitpointID:    waitpointID,
		checkpointID:   checkpointID,
		queueMessageID: queueMessageID,
	}
}

func requireWaitpointContinuationDispatchable(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID, runID pgtype.UUID) db.PrepareQueuedRunQueueEntryRow {
	t.Helper()
	candidates, err := queries.ListQueuedRunQueueEntryCandidates(ctx, db.ListQueuedRunQueueEntryCandidatesParams{
		OrgID:    orgID,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.RunID == runID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("run %v not in queue candidates: %+v", runID, candidates)
	}
	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RunID != runID {
		t.Fatalf("prepared run id = %v, want %v", prepared.RunID, runID)
	}
	return prepared
}

func TestLeaseRunExecutionRejectsMismatchedWorkerRuntimeAndPlacement(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		heartbeat string
		region    string
		labels    string
	}{
		{
			name:      "runtime_arch",
			heartbeat: `{"runtime_arch":"arm64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "runtime_abi",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"other","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "kernel_digest",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:other-kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "rootfs_digest",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:other-rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "cni_profile",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"other/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "region",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-west-2",
			labels:    `{"pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
		{
			name:      "labels",
			heartbeat: `{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`,
			region:    "us-east-1",
			labels:    `{"pool":"gpu","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries, pool := newPostgresTestDB(t, ctx)
			orgID := ids.ToPG(ids.DefaultOrgID)

			if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
				t.Fatal(err)
			}
			scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
			if err != nil {
				t.Fatal(err)
			}
			group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "mismatch-"+tt.name, "queue-mismatch-"+tt.name)
			host := upsertTestWorkerHostWithRuntime(t, ctx, queries, orgID, group.ID, "runner-"+tt.name, tt.region, []byte(tt.labels), []byte(tt.heartbeat))
			runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

			if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
				RunID:                   runID,
				OrgID:                   orgID,
				WorkerGroupID:           group.ID,
				RequestedMilliCpu:       1000,
				RequestedMemoryMib:      1024,
				RequestedDiskMib:        2048,
				RequestedExecutionSlots: 1,
				RuntimeArch:             "x86_64",
				RuntimeABI:              "helmr.firecracker.snapshot.v0",
				KernelDigest:            "sha256:kernel",
				RootfsDigest:            "sha256:rootfs",
				CniProfile:              "helmr/v1",
				NetworkPolicy:           []byte(`{}`),
				Placement:               []byte(`{"region":"us-east-1","tags":{"pool":"standard"},"dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
				RunID:          runID,
				OrgID:          orgID,
				WorkerGroupID:  group.ID,
				Priority:       10,
				QueueName:      group.QueueName,
				QueueMessageID: "message-" + tt.name,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.MarkRunQueueEntryLeased(ctx, db.MarkRunQueueEntryLeasedParams{
				OrgID:          orgID,
				RunID:          runID,
				WorkerGroupID:  group.ID,
				WorkerHostID:   host.ID,
				QueueMessageID: "message-" + tt.name,
				LeaseExpiresAt: pgTime(time.Now().Add(time.Minute)),
			}); err != nil {
				t.Fatal(err)
			}

			_, err = queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
				OrgID:           orgID,
				RunID:           runID,
				WorkerGroupID:   group.ID,
				WorkerHostID:    host.ID,
				ExecutionID:     ids.ToPG(ids.New()),
				QueueMessageID:  "message-" + tt.name,
				QueueLeaseID:    "lease-" + tt.name,
				DeliveryAttempt: 1,
				LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("lease error = %v, want no rows", err)
			}

			var executionCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_executions WHERE org_id = $1 AND run_id = $2`, orgID, runID).Scan(&executionCount); err != nil {
				t.Fatal(err)
			}
			if executionCount != 0 {
				t.Fatalf("run executions = %d, want 0", executionCount)
			}
			var runStatus db.RunStatus
			var currentExecutionID pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT status, current_execution_id FROM runs WHERE org_id = $1 AND id = $2`, orgID, runID).Scan(&runStatus, &currentExecutionID); err != nil {
				t.Fatal(err)
			}
			if runStatus != db.RunStatusQueued || currentExecutionID.Valid {
				t.Fatalf("run status/current_execution = %q/%v, want queued/null", runStatus, currentExecutionID)
			}
		})
	}
}

func TestDeadLetterRunQueueEntryFailsQueuedRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "dead-letter", "dead-letter-queue")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           group.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerGroupID:  group.ID,
		Priority:       10,
		QueueName:      group.QueueName,
		QueueMessageID: "message-dead-letter",
	}); err != nil {
		t.Fatal(err)
	}

	entry, err := queries.DeadLetterRunQueueEntry(ctx, db.DeadLetterRunQueueEntryParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  group.ID,
		QueueMessageID: "message-dead-letter",
		LastError:      "run exceeded max delivery attempts (5)",
		EventKind:      "run.dead_lettered",
		EventPayload:   []byte(`{"reason":"max_delivery_attempts_exceeded"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != db.RunQueueStatusDeadLettered || !entry.FinishedAt.Valid {
		t.Fatalf("queue status/finished_at = %q/%v, want dead_lettered/set", entry.Status, entry.FinishedAt)
	}
	var runStatus db.RunStatus
	var errorMessage string
	if err := pool.QueryRow(ctx, `SELECT status, error_message FROM runs WHERE org_id = $1 AND id = $2`, orgID, runID).Scan(&runStatus, &errorMessage); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusFailed || errorMessage == "" {
		t.Fatalf("run status/error = %q/%q, want failed/error", runStatus, errorMessage)
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE org_id = $1 AND run_id = $2 AND kind = 'run.dead_lettered'`, orgID, runID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("dead-letter event count = %d, want 1", eventCount)
	}
}

func TestFailExpiredRunningRunExecutionsCompletesLeasedQueueEntry(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	if err := queries.EnsureDefaultOrganization(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "expired-running", "expired-running-queue")
	host := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "runner-expired")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           group.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v1",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerGroupID:  group.ID,
		Priority:       10,
		QueueName:      group.QueueName,
		QueueMessageID: "message-expired-running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryLeased(ctx, db.MarkRunQueueEntryLeasedParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  group.ID,
		WorkerHostID:   host.ID,
		QueueMessageID: "message-expired-running",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	executionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecution(ctx, db.LeaseRunExecutionParams{
		OrgID:           orgID,
		RunID:           runID,
		WorkerGroupID:   group.ID,
		WorkerHostID:    host.ID,
		ExecutionID:     executionID,
		QueueMessageID:  "message-expired-running",
		QueueLeaseID:    "lease-expired-running",
		DeliveryAttempt: 1,
		LeaseExpiresAt:  pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:         orgID,
		RunID:         runID,
		ExecutionID:   executionID,
		WorkerGroupID: group.ID,
		WorkerHostID:  host.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_executions SET lease_expires_at = now() - interval '1 second' WHERE org_id = $1 AND id = $2`, orgID, executionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunExecutions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	var runStatus db.RunStatus
	var currentExecutionID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, current_execution_id FROM runs WHERE org_id = $1 AND id = $2`, orgID, runID).Scan(&runStatus, &currentExecutionID); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusFailed || currentExecutionID.Valid {
		t.Fatalf("run status/current_execution = %q/%v, want failed/null", runStatus, currentExecutionID)
	}

	var executionStatus db.RunExecutionStatus
	var lostAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT status, lost_at FROM run_executions WHERE org_id = $1 AND id = $2`, orgID, executionID).Scan(&executionStatus, &lostAt); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusLost || !lostAt.Valid {
		t.Fatalf("execution status/lost_at = %q/%v, want lost/set", executionStatus, lostAt)
	}

	var queueStatus db.RunQueueStatus
	var finishedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT status, finished_at FROM run_queue_entries WHERE org_id = $1 AND run_id = $2`, orgID, runID).Scan(&queueStatus, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if queueStatus != db.RunQueueStatusCompleted || !finishedAt.Valid {
		t.Fatalf("queue status/finished_at = %q/%v, want completed/set", queueStatus, finishedAt)
	}
}
