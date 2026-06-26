package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSessionLoserRunIsNotVisibleOrLeaseable(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	workspaceID := ids.workspaceID
	baseArtifactID := uuid.Must(uuid.NewV7())
	baseVersionID := uuid.Must(uuid.NewV7())
	baseDigest := "sha256:" + strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 10, 'application/vnd.helmr.workspace.v0.tar')
		ON CONFLICT DO NOTHING
	`, baseDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'workspace_version', 10, 'application/vnd.helmr.workspace.v0.tar')
	`, baseArtifactID, ids.orgID, ids.projectID, ids.environmentID, baseDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, artifact_id,
			artifact_encoding, artifact_entry_count, content_digest, size_bytes, state, promoted_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'tar', 1, $7, 10, 'ready', now())
	`, baseVersionID, ids.orgID, ids.projectID, ids.environmentID, workspaceID, baseArtifactID, baseDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, baseVersionID, ids.orgID, workspaceID); err != nil {
		t.Fatal(err)
	}
	loserRunID := uuid.Must(uuid.NewV7())
	loserAttemptID := uuid.Must(uuid.NewV7())
	winnerAttemptID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	dispatchMessageID := "dispatch-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	winnerDispatchMessageID := "dispatch-" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := queries.EnsureRuntimeReleaseSelection(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, 'active', $4,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$5, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE deployments
		   SET worker_protocol_version = $1
		 WHERE org_id = $2
		   AND id = $3
	`, api.CurrentWorkerProtocolVersion, ids.orgID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'queued')
	`, winnerAttemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       queue_timestamp = now(),
		       current_attempt_id = $3,
		       current_attempt_number = 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID, winnerAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1, 1, 1, 1, $3, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, queue_timestamp,
			max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task',
			$8, 'queued', 'queued', '{}', 'default', now(), 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, loserRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'queued')
	`, loserAttemptID, ids.orgID, loserRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_attempt_id = $1,
		       current_attempt_number = 1
		 WHERE org_id = $2
		   AND id = $3
	`, loserAttemptID, ids.orgID, loserRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1, 1, 1, 1, $3, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, loserRunID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, 'reserved', 'default', $3, $4, now() + interval '1 hour')
	`, loserRunID, ids.orgID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	visible, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		QueueName:     "default",
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range visible {
		if pgvalue.MustUUIDValue(row.RunID) == loserRunID {
			t.Fatalf("loser run %s was queue-visible", loserRunID)
		}
	}
	var winnerVisible bool
	for _, row := range visible {
		if pgvalue.MustUUIDValue(row.RunID) == ids.runID {
			winnerVisible = true
			break
		}
	}
	if !winnerVisible {
		t.Fatalf("current winner run %s was not queue-visible", ids.runID)
	}
	_, err = queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(loserRunID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("PrepareQueuedRunQueueItem error = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(loserRunID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: dispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-1",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "3333333333333333",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseRunLease error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_queue_items
		   SET status = 'published',
		       reserved_by_worker_instance_id = NULL,
		       reservation_expires_at = NULL
		 WHERE org_id = $1
		   AND run_id = $2
	`, ids.orgID, loserRunID); err != nil {
		t.Fatal(err)
	}
	_, err = queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(loserRunID),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		WorkerInstanceID:     pgvalue.UUID(workerID),
		DispatchMessageID:    pgtype.Text{String: dispatchMessageID, Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ReserveRunQueueItem loser error = %v, want pgx.ErrNoRows", err)
	}
	preparedWinner, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(preparedWinner.RunID) != ids.runID {
		t.Fatalf("prepared run = %s, want %s", pgvalue.MustUUIDValue(preparedWinner.RunID), ids.runID)
	}
	if _, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(ids.runID),
		DispatchMessageID:          pgtype.Text{String: winnerDispatchMessageID, Valid: true},
		ExpectedDispatchGeneration: preparedWinner.DispatchGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	reservedWinner, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(ids.runID),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		WorkerInstanceID:     pgvalue.UUID(workerID),
		DispatchMessageID:    pgtype.Text{String: winnerDispatchMessageID, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(reservedWinner.RunID) != ids.runID {
		t.Fatalf("reserved run = %s, want %s", pgvalue.MustUUIDValue(reservedWinner.RunID), ids.runID)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET queue_concurrency_limit = 1,
		       concurrency_key = 'workspace-first'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	_, err = queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: winnerDispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-current",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "4444444444444444",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseRunLease without live materialization error = %v, want pgx.ErrNoRows", err)
	}
	var activeConcurrencySlots int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM run_queue_concurrency_leases
		 WHERE org_id = $1
		   AND run_id = $2
		   AND released_at IS NULL
	`, ids.orgID, ids.runID).Scan(&activeConcurrencySlots); err != nil {
		t.Fatal(err)
	}
	if activeConcurrencySlots != 0 {
		t.Fatalf("LeaseRunLease without live materialization active concurrency slots = %d, want 0", activeConcurrencySlots)
	}
	var materializationCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializationCount); err != nil {
		t.Fatal(err)
	}
	if materializationCount != 0 {
		t.Fatalf("LeaseRunLease created materializations = %d, want 0", materializationCount)
	}
	requestedMaterialization, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimedMaterialization, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      1000,
		AvailableMemoryMib:      1024,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            "sha256:rootfs",
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "materialization-reservation-token",
		ReservationExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:  "materialization-channel-token-hash",
		RuntimeID:               runtimeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedMaterialization.ID != claimedMaterialization.ID {
		t.Fatalf("claimed materialization id = %v, want %v", claimedMaterialization.ID, requestedMaterialization.ID)
	}
	if _, err := queries.MarkWorkspaceMaterializationRunning(ctx, db.MarkWorkspaceMaterializationRunningParams{
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   claimedMaterialization.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "materialization-reservation-token",
	}); err != nil {
		t.Fatal(err)
	}
	leasedWinner, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: winnerDispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-current",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "4444444444444444",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(leasedWinner.ID) != ids.runID {
		t.Fatalf("leased run = %s, want %s", pgvalue.MustUUIDValue(leasedWinner.ID), ids.runID)
	}
	if !leasedWinner.WorkspaceID.Valid || !leasedWinner.WorkspaceLeaseID.Valid {
		t.Fatalf("leased winner workspace = id:%+v lease:%+v, want write lease", leasedWinner.WorkspaceID, leasedWinner.WorkspaceLeaseID)
	}
	if !leasedWinner.WorkspaceFencingToken.Valid || strings.TrimSpace(leasedWinner.WorkspaceFencingToken.String) == "" {
		t.Fatalf("leased winner workspace fencing token = %+v, want token", leasedWinner.WorkspaceFencingToken)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET started_at = now() - interval '2 seconds',
		       leased_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.RunLeaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	var acquiredFencingGeneration int64
	var materializationFencingGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT workspace_leases.acquired_fencing_generation,
		       workspace_materializations.fencing_generation
		  FROM workspace_leases
		  JOIN workspace_materializations
		    ON workspace_materializations.org_id = workspace_leases.org_id
		   AND workspace_materializations.id = workspace_leases.materialization_id
		 WHERE workspace_leases.org_id = $1
		   AND workspace_leases.id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceLeaseID)).Scan(&acquiredFencingGeneration, &materializationFencingGeneration); err != nil {
		t.Fatal(err)
	}
	if acquiredFencingGeneration != materializationFencingGeneration || acquiredFencingGeneration <= 1 {
		t.Fatalf("workspace fencing generations lease=%d materialization=%d, want matching incremented generation", acquiredFencingGeneration, materializationFencingGeneration)
	}
	renewedExpiresAt := pgtype.Timestamptz{Time: time.Now().Add(2 * time.Hour), Valid: true}
	if _, err := queries.RenewRunLease(ctx, db.RenewRunLeaseParams{
		LeaseExpiresAt:    renewedExpiresAt,
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        leasedWinner.RunLeaseID,
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: winnerDispatchMessageID,
		DispatchLeaseID:   "lease-current",
	}); err != nil {
		t.Fatal(err)
	}
	var workspaceLeaseExpiresAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT expires_at
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceLeaseID)).Scan(&workspaceLeaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if !workspaceLeaseExpiresAt.Valid || workspaceLeaseExpiresAt.Time.Sub(renewedExpiresAt.Time).Abs() > time.Second {
		t.Fatalf("workspace lease expires_at = %+v, want %s", workspaceLeaseExpiresAt, renewedExpiresAt.Time)
	}
	workspaceDigest := "sha256:" + strings.Repeat("b", 64)
	_, err = queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                       pgvalue.UUID(ids.orgID),
		RunID:                       pgvalue.UUID(ids.runID),
		RunLeaseID:                  leasedWinner.RunLeaseID,
		WorkerInstanceID:            pgvalue.UUID(workerID),
		DispatchMessageID:           winnerDispatchMessageID,
		DispatchLeaseID:             "lease-current",
		RunStatus:                   db.RunStatusSucceeded,
		WorkspaceLeaseID:            leasedWinner.WorkspaceLeaseID,
		WorkspaceFencingToken:       pgvalue.Text("stale-fencing-token"),
		WorkspaceArtifactDigest:     pgvalue.Text(workspaceDigest),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		WorkspaceBaseVersionID:      leasedWinner.WorkspaceBaseVersionID,
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      []byte(`{"ok":true}`),
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{"status":"succeeded"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ReleaseRunLease stale fencing token error = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                       pgvalue.UUID(ids.orgID),
		RunID:                       pgvalue.UUID(ids.runID),
		RunLeaseID:                  leasedWinner.RunLeaseID,
		WorkerInstanceID:            pgvalue.UUID(workerID),
		DispatchMessageID:           winnerDispatchMessageID,
		DispatchLeaseID:             "lease-current",
		RunStatus:                   db.RunStatusSucceeded,
		WorkspaceLeaseID:            leasedWinner.WorkspaceLeaseID,
		WorkspaceFencingToken:       leasedWinner.WorkspaceFencingToken,
		WorkspaceArtifactDigest:     pgvalue.Text(workspaceDigest),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		WorkspaceBaseVersionID:      pgvalue.UUID(uuid.Must(uuid.NewV7())),
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      []byte(`{"ok":true}`),
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{"status":"succeeded"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ReleaseRunLease forged base version error = %v, want pgx.ErrNoRows", err)
	}
	released, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                       pgvalue.UUID(ids.orgID),
		RunID:                       pgvalue.UUID(ids.runID),
		RunLeaseID:                  leasedWinner.RunLeaseID,
		WorkerInstanceID:            pgvalue.UUID(workerID),
		DispatchMessageID:           winnerDispatchMessageID,
		DispatchLeaseID:             "lease-current",
		RunStatus:                   db.RunStatusSucceeded,
		WorkspaceLeaseID:            leasedWinner.WorkspaceLeaseID,
		WorkspaceFencingToken:       leasedWinner.WorkspaceFencingToken,
		WorkspaceArtifactDigest:     pgvalue.Text(workspaceDigest),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		WorkspaceBaseVersionID:      leasedWinner.WorkspaceBaseVersionID,
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      []byte(`{"ok":true}`),
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{"status":"succeeded"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusSucceeded {
		t.Fatalf("released status = %s, want succeeded", released.Status)
	}
	if released.ActiveElapsedMs < 2000 || released.ActiveElapsedMs >= released.MaxActiveDurationMs {
		t.Fatalf("active elapsed ms = %d max = %d, want DB lease-clock elapsed below max", released.ActiveElapsedMs, released.MaxActiveDurationMs)
	}
	if released.ActiveStartedAt.Valid {
		t.Fatalf("active_started_at = %+v, want closed active interval", released.ActiveStartedAt)
	}
	var leaseState db.WorkspaceLeaseState
	var leaseReleasedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT state, released_at
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceLeaseID)).Scan(&leaseState, &leaseReleasedAt); err != nil {
		t.Fatal(err)
	}
	if leaseState != db.WorkspaceLeaseStateReleased || !leaseReleasedAt.Valid {
		t.Fatalf("workspace lease state=%s released_at_valid=%v, want released/valid", leaseState, leaseReleasedAt.Valid)
	}
	var currentVersionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceID)).Scan(&currentVersionID); err != nil {
		t.Fatal(err)
	}
	if !currentVersionID.Valid {
		t.Fatal("workspace current_version_id is null after successful release")
	}
	var versionDigest string
	var entryCount int32
	if err := pool.QueryRow(ctx, `
		SELECT artifacts.digest, workspace_versions.artifact_entry_count
		  FROM workspace_versions
		  JOIN artifacts ON artifacts.org_id = workspace_versions.org_id
		                AND artifacts.project_id = workspace_versions.project_id
		                AND artifacts.environment_id = workspace_versions.environment_id
		                AND artifacts.id = workspace_versions.artifact_id
		 WHERE workspace_versions.org_id = $1
		   AND workspace_versions.workspace_id = $2
		   AND workspace_versions.id = $3
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceID), pgvalue.MustUUIDValue(currentVersionID)).Scan(&versionDigest, &entryCount); err != nil {
		t.Fatal(err)
	}
	if versionDigest != workspaceDigest || entryCount != 2 {
		t.Fatalf("workspace version digest=%s entry_count=%d, want %s/2", versionDigest, entryCount, workspaceDigest)
	}
}

func TestLeaseRunLeaseRejectsStaleRuntimeCheckpointWithoutLeakingLeases(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	runLeaseID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	materializationID := uuid.Must(uuid.NewV7())
	sourceWorkspaceLeaseID := uuid.Must(uuid.NewV7())
	staleCheckpointID := uuid.Must(uuid.NewV7())
	staleArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	staleVersionID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dispatchMessageID := "dispatch-" + shortUUID(runLeaseID)
	workerResourceID := "worker-" + shortUUID(workerID)
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
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, 'active', $4,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$5, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE deployments
		   SET worker_protocol_version = $1
		 WHERE org_id = $2
		   AND id = $3
	`, api.CurrentWorkerProtocolVersion, ids.orgID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $6
	`, staleVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, staleArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_materializations (
			id, org_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
			worker_instance_id, reservation_expires_at, requested_cpu_millis, requested_memory_mib,
			requested_disk_mib, requested_execution_slots, reserved_cpu_millis, reserved_memory_mib,
			reserved_disk_mib, reserved_execution_slots, image_artifact_id, image_artifact_format,
			rootfs_digest, image_digest, image_format, workspace_artifact_id, workspace_artifact_encoding,
			workspace_artifact_entry_count, workspace_artifact_digest, workspace_artifact_size_bytes,
			workspace_artifact_media_type, workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state
		)
		SELECT $1, workspaces.org_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
		       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
		       $2, now() + interval '1 hour', 1000, 1024, 4096, 1, 0, 0, 0, 0,
		       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
		       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
		       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
		       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'running'
		  FROM workspaces
		  JOIN deployment_sandboxes
		    ON deployment_sandboxes.org_id = workspaces.org_id
		   AND deployment_sandboxes.project_id = workspaces.project_id
		   AND deployment_sandboxes.environment_id = workspaces.environment_id
		   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
		  JOIN artifacts AS image_artifact
		    ON image_artifact.org_id = deployment_sandboxes.org_id
		   AND image_artifact.project_id = deployment_sandboxes.project_id
		   AND image_artifact.environment_id = deployment_sandboxes.environment_id
		   AND image_artifact.id = deployment_sandboxes.image_artifact_id
		  JOIN workspace_versions
		    ON workspace_versions.org_id = workspaces.org_id
		   AND workspace_versions.project_id = workspaces.project_id
		   AND workspace_versions.environment_id = workspaces.environment_id
		   AND workspace_versions.workspace_id = workspaces.id
		   AND workspace_versions.id = workspaces.current_version_id
		  JOIN artifacts AS workspace_artifact
		    ON workspace_artifact.org_id = workspace_versions.org_id
		   AND workspace_artifact.project_id = workspace_versions.project_id
		   AND workspace_artifact.environment_id = workspace_versions.environment_id
		   AND workspace_artifact.id = workspace_versions.artifact_id
		 WHERE workspaces.org_id = $3
		   AND workspaces.id = $4
	`, materializationID, workerID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at, released_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'write', 'released', $7, $8, $8, 1,
		        'stale-checkpoint-source-lease', now() + interval '1 hour', now())
	`, sourceWorkspaceLeaseID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, materializationID, ids.runID, staleVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, materialization_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest, ready_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		        'ready', 'test', $10, 'arm64', 'test', 'sha256:kernel',
		        'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'default', '{}', now())
	`, staleCheckpointID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, ids.runID, sourceWorkspaceLeaseID, materializationID, staleVersionID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'queued')
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued',
		       queue_timestamp = now(),
		       current_attempt_id = $1,
		       current_attempt_number = 1,
		       latest_runtime_checkpoint_id = $2,
		       queue_concurrency_limit = 1,
		       concurrency_key = 'stale-checkpoint'
		 WHERE org_id = $3
		   AND id = $4
	`, attemptID, staleCheckpointID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1, 1, 1, 1, $3, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, 'reserved', 'default', $3, $4, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		DispatchMessageID: pgtype.Text{String: dispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-" + shortUUID(runLeaseID),
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "3333333333333333",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseRunLease stale checkpoint error = %v, want pgx.ErrNoRows", err)
	}
	var activeConcurrencySlots int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM run_queue_concurrency_leases
		 WHERE org_id = $1
		   AND run_id = $2
		   AND released_at IS NULL
	`, ids.orgID, ids.runID).Scan(&activeConcurrencySlots); err != nil {
		t.Fatal(err)
	}
	if activeConcurrencySlots != 0 {
		t.Fatalf("active concurrency slots after stale checkpoint lease failure = %d, want 0", activeConcurrencySlots)
	}
	var activeWorkspaceLeases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND lease_kind = 'write'
		   AND state IN ('active', 'releasing')
		   AND released_at IS NULL
	`, ids.orgID, ids.workspaceID).Scan(&activeWorkspaceLeases); err != nil {
		t.Fatal(err)
	}
	if activeWorkspaceLeases != 0 {
		t.Fatalf("active workspace write leases after stale checkpoint lease failure = %d, want 0", activeWorkspaceLeases)
	}
}

func TestReleaseLeasedRunLeaseDoesNotAccrueActiveTimeBeforeStart(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'leased',
		       started_at = NULL,
		       leased_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET execution_status = 'leased',
		       active_elapsed_ms = 500,
		       active_started_at = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	released, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		RunID:                 pgvalue.UUID(ids.runID),
		RunLeaseID:            pgvalue.UUID(runLeaseID),
		WorkerInstanceID:      pgvalue.UUID(workerID),
		DispatchMessageID:     "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:       "lease-" + runLeaseID.String()[:8],
		RunStatus:             db.RunStatusFailed,
		AttemptStatus:         db.RunAttemptStatusFailed,
		ExitCode:              pgtype.Int4{},
		ErrorMessage:          pgtype.Text{String: "payload build failed", Valid: true},
		TerminalEventKind:     "run.failed",
		TerminalEventPayload:  []byte(`{"status":"failed"}`),
		WorkspaceFencingToken: pgtype.Text{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.ActiveElapsedMs != 500 {
		t.Fatalf("active elapsed ms = %d, want previous value without leased-time accrual", released.ActiveElapsedMs)
	}
	if released.ActiveStartedAt.Valid {
		t.Fatalf("active_started_at = %+v, want closed nil interval", released.ActiveStartedAt)
	}
}

func TestReleaseRunLeaseDoesNotRegressActiveTimeWhenClockMovesBackward(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() + interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	released, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		RunID:                 pgvalue.UUID(ids.runID),
		RunLeaseID:            pgvalue.UUID(runLeaseID),
		WorkerInstanceID:      pgvalue.UUID(workerID),
		DispatchMessageID:     "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:       "lease-" + runLeaseID.String()[:8],
		RunStatus:             db.RunStatusFailed,
		AttemptStatus:         db.RunAttemptStatusFailed,
		ExitCode:              pgtype.Int4{},
		ErrorMessage:          pgtype.Text{String: "clock skew regression", Valid: true},
		TerminalEventKind:     "run.failed",
		TerminalEventPayload:  []byte(`{"status":"failed"}`),
		WorkspaceFencingToken: pgtype.Text{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.ActiveElapsedMs != 500 {
		t.Fatalf("active elapsed ms = %d, want previous value under negative elapsed interval", released.ActiveElapsedMs)
	}
	if released.ActiveStartedAt.Valid {
		t.Fatalf("active_started_at = %+v, want closed active interval", released.ActiveStartedAt)
	}
}
