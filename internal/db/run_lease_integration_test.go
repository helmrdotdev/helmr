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
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 10, 'application/vnd.helmr.workspace.v0.tar')
		ON CONFLICT DO NOTHING
	`, ids.orgID, baseDigest); err != nil {
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
			id, public_id, org_id, worker_group_id, project_id, environment_id, workspace_id, artifact_id,
			artifact_encoding, artifact_entry_count, content_digest, size_bytes, state, promoted_at
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, $7, 'tar', 1, $8, 10, 'ready', now())
	`, baseVersionID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, workspaceID, baseArtifactID, baseDigest, testWorkspaceVersionPublicID(t)); err != nil {
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
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE id = $1 AND name = 'default'`, dbtest.DefaultWorkerGroupID).Scan(&workerGroupID); err != nil {
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
			id, org_id, worker_group_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$7, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultWorkerGroupID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
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
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, winnerAttemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
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
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, $3, 1, 1, 1, 1, $4, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $5)
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, public_id, org_id, worker_group_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, queue_timestamp,
			max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task',
			$9, 'queued', 'queued', '{}', 'default', now(), 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, loserRunID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID, testRunPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, loserAttemptID, ids.orgID, dbtest.DefaultWorkerGroupID, loserRunID); err != nil {
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
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, $3, 1, 1, 1, 1, $4, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $5)
	`, loserRunID, ids.orgID, dbtest.DefaultWorkerGroupID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, loserRunID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	visible, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		QueueClass:    "default",
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
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		RunID:                pgvalue.UUID(loserRunID),
		QueueClass:           "default",
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
		WorkerGroupID:              preparedWinner.WorkerGroupID,
		QueueClass:                 preparedWinner.QueueClass,
		RunID:                      pgvalue.UUID(ids.runID),
		DispatchMessageID:          pgtype.Text{String: winnerDispatchMessageID, Valid: true},
		ExpectedDispatchGeneration: preparedWinner.DispatchGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	reservedWinner, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		WorkerGroupID:        preparedWinner.WorkerGroupID,
		RunID:                pgvalue.UUID(ids.runID),
		QueueClass:           preparedWinner.QueueClass,
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
		t.Fatalf("LeaseRunLease without live workspaceMount error = %v, want pgx.ErrNoRows", err)
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
		t.Fatalf("LeaseRunLease without live workspaceMount active concurrency slots = %d, want 0", activeConcurrencySlots)
	}
	var workspaceMountCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_mounts
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceMountCount); err != nil {
		t.Fatal(err)
	}
	if workspaceMountCount != 0 {
		t.Fatalf("LeaseRunLease created workspaceMounts = %d, want 0", workspaceMountCount)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimedMount, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "runtime-instance-token",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedMount.ID != claimedMount.ID {
		t.Fatalf("claimed workspace mount id = %v, want %v", claimedMount.ID, requestedMount.ID)
	}
	if _, err := queries.MarkWorkspaceMountMounted(ctx, db.MarkWorkspaceMountMountedParams{
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		OrgID:                       pgvalue.UUID(ids.orgID),
		ID:                          claimedMount.ID,
		WorkerInstanceID:            pgvalue.UUID(workerID),
		RuntimeInstanceToken:        claimedMount.RuntimeInstanceToken,
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
	var leasedRuntimeEpoch int64
	if err := pool.QueryRow(ctx, `
		SELECT runtime_epoch
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
		`, ids.orgID, pgvalue.MustUUIDValue(claimedMount.RuntimeInstanceID)).Scan(&leasedRuntimeEpoch); err != nil {
		t.Fatal(err)
	}
	if leasedRuntimeEpoch != 2 {
		t.Fatalf("leased runtime epoch = %d, want 2", leasedRuntimeEpoch)
	}
	if strings.TrimSpace(leasedWinner.WorkspaceFencingToken) == "" {
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
	var workspaceMountFencingGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT workspace_leases.acquired_fencing_generation,
		       workspace_mounts.fencing_generation
		  FROM workspace_leases
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = workspace_leases.org_id
		   AND workspace_mounts.id = workspace_leases.workspace_mount_id
		 WHERE workspace_leases.org_id = $1
		   AND workspace_leases.id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(leasedWinner.WorkspaceLeaseID)).Scan(&acquiredFencingGeneration, &workspaceMountFencingGeneration); err != nil {
		t.Fatal(err)
	}
	if acquiredFencingGeneration != workspaceMountFencingGeneration || acquiredFencingGeneration <= 1 {
		t.Fatalf("workspace fencing generations lease=%d workspaceMount=%d, want matching incremented generation", acquiredFencingGeneration, workspaceMountFencingGeneration)
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
		WorkspaceVersionPublicID:    testWorkspaceVersionPublicID(t),
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
		WorkspaceFencingToken:       pgvalue.Text(leasedWinner.WorkspaceFencingToken),
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
		WorkspaceVersionPublicID:    testWorkspaceVersionPublicID(t),
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
		WorkspaceFencingToken:       pgvalue.Text(leasedWinner.WorkspaceFencingToken),
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
		WorkspaceVersionPublicID:    testWorkspaceVersionPublicID(t),
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
	var runtimeState db.RuntimeInstanceState
	var ownerRunID pgtype.UUID
	var ownerRunLeaseID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT state, owner_run_id, owner_run_lease_id
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
		`, ids.orgID, pgvalue.MustUUIDValue(claimedMount.RuntimeInstanceID)).Scan(&runtimeState, &ownerRunID, &ownerRunLeaseID); err != nil {
		t.Fatal(err)
	}
	if runtimeState != db.RuntimeInstanceStateWaitingHot || ownerRunID.Valid || ownerRunLeaseID.Valid {
		t.Fatalf("runtime after terminal release = state %s owner_run_valid=%v owner_lease_valid=%v, want waiting_hot/no owner", runtimeState, ownerRunID.Valid, ownerRunLeaseID.Valid)
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
	workspaceMountID := uuid.Must(uuid.NewV7())
	sourceWorkspaceLeaseID := uuid.Must(uuid.NewV7())
	staleCheckpointID := uuid.Must(uuid.NewV7())
	staleArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	staleVersionID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dispatchMessageID := "dispatch-" + shortUUID(runLeaseID)
	workerResourceID := "worker-" + shortUUID(workerID)
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE id = $1 AND name = 'default'`, dbtest.DefaultWorkerGroupID).Scan(&workerGroupID); err != nil {
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
			id, org_id, worker_group_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$7, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultWorkerGroupID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
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
			id, public_id, org_id, worker_group_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $8, $2, $3, $4, $5, $6, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $4
		   AND artifacts.environment_id = $5
		   AND artifacts.id = $7
	`, staleVersionID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.workspaceID, staleArtifactID, testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_artifact_id, workspace_artifact_encoding,
			workspace_artifact_entry_count, workspace_artifact_digest, workspace_artifact_size_bytes,
			workspace_artifact_media_type, workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state,
			mounted_at
		)
		SELECT $1, workspaces.org_id, workspaces.worker_group_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
		       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
		       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
		       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
		       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
		       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted', now()
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
		 WHERE workspaces.org_id = $2
		   AND workspaces.id = $3
	`, workspaceMountID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at, released_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'write', 'released', $8, $9, $9, 1,
		        'stale-checkpoint-source-lease', now() + interval '1 hour', now())
	`, sourceWorkspaceLeaseID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.workspaceID, workspaceMountID, ids.runID, staleVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest, ready_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		        'ready', 'test', $11, 'arm64', 'test', 'sha256:kernel',
		        'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'default', '{}', now())
	`, staleCheckpointID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.workspaceID, ids.runID, sourceWorkspaceLeaseID, workspaceMountID, staleVersionID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, attemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
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
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, $3, 1, 1, 1, 1, $4, 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', $5)
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
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

func TestLeaseRunLeaseCreditsResidentRuntimeOnOneSlotWorker(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID := seedRuntimePressureWorker(t, ctx, pool, ids, 1000, 1024, 4096, 1)
	dispatchMessageID := "dispatch-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	attemptID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, attemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
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
	`, ids.orgID, ids.runID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		SELECT $1, $2, worker_instances.worker_group_id, 1000, 1024, 4096, 1, worker_instances.runtime_id, worker_instances.runtime_arch,
		       worker_instances.runtime_abi, worker_instances.kernel_digest, worker_instances.initramfs_digest,
		       worker_instances.rootfs_digest, worker_instances.cni_profile, worker_instances.worker_group_id
		  FROM worker_instances
		 WHERE worker_instances.id = $3
	`, ids.runID, ids.orgID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}

	workspaceMountID := uuid.Must(uuid.NewV7())
	seedResidentRuntimeWorkspaceMount(t, ctx, pool, ids, ids.workspaceID, workspaceMountID, workerID, 1000, 1024, 4096, 1)
	leased, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: dispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-with-resident",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "4444444444444444",
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.WorkspaceMountID != pgvalue.UUID(workspaceMountID) {
		t.Fatalf("workspace mount id = %v, want resident %s", leased.WorkspaceMountID, workspaceMountID)
	}
	if leased.RunLeaseWorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("run lease worker = %v, want %s", leased.RunLeaseWorkerInstanceID, workerID)
	}
	if !leased.WorkspaceLeaseID.Valid || strings.TrimSpace(leased.WorkspaceFencingToken) == "" {
		t.Fatalf("workspace lease id/token = %+v/%q, want resident write lease", leased.WorkspaceLeaseID, leased.WorkspaceFencingToken)
	}
}

func TestLeaseRunLeaseDoesNotReturnWrongWorkerGroupRuntimeSubstrate(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID := seedRuntimePressureWorker(t, ctx, pool, ids, 1000, 1024, 4096, 1)
	dispatchMessageID := "dispatch-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	attemptID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, attemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
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
	`, ids.orgID, ids.runID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		SELECT $1, $2, worker_instances.worker_group_id, 1000, 1024, 4096, 1, worker_instances.runtime_id, worker_instances.runtime_arch,
		       worker_instances.runtime_abi, worker_instances.kernel_digest, worker_instances.initramfs_digest,
		       worker_instances.rootfs_digest, worker_instances.cni_profile, worker_instances.worker_group_id
		  FROM worker_instances
		 WHERE worker_instances.id = $3
	`, ids.runID, ids.orgID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}

	workspaceMountID := uuid.Must(uuid.NewV7())
	seedResidentRuntimeWorkspaceMount(t, ctx, pool, ids, ids.workspaceID, workspaceMountID, workerID, 1000, 1024, 4096, 1)
	seedWrongWorkerGroupRuntimeSubstrateArtifact(t, ctx, pool, queries, ids)

	leased, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: dispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-with-wrong-worker-group-substrate",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "6666666666666666",
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.WorkspaceMountID != pgvalue.UUID(workspaceMountID) {
		t.Fatalf("workspace mount id = %v, want resident %s", leased.WorkspaceMountID, workspaceMountID)
	}
	if leased.WorkspaceRuntimeSubstrateArtifactID.Valid {
		t.Fatalf("workspace runtime substrate artifact id = %+v, want absent wrong-worker-group substrate", leased.WorkspaceRuntimeSubstrateArtifactID)
	}
	if leased.WorkspaceRuntimeSubstrateDigest != "" || leased.WorkspaceRuntimeSubstrateArtifactDigest != "" {
		t.Fatalf("workspace runtime substrate metadata = %q/%q, want empty wrong-worker-group substrate metadata", leased.WorkspaceRuntimeSubstrateDigest, leased.WorkspaceRuntimeSubstrateArtifactDigest)
	}
}

func TestLeaseRunLeaseDoesNotReclaimCheckpointingResidentRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID := seedRuntimePressureWorker(t, ctx, pool, ids, 1000, 1024, 4096, 1)
	dispatchMessageID := "dispatch-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	attemptID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'queued')
	`, attemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
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
	`, ids.orgID, ids.runID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		SELECT $1, $2, worker_instances.worker_group_id, 1000, 1024, 4096, 1, worker_instances.runtime_id, worker_instances.runtime_arch,
		       worker_instances.runtime_abi, worker_instances.kernel_digest, worker_instances.initramfs_digest,
		       worker_instances.rootfs_digest, worker_instances.cni_profile, worker_instances.worker_group_id
		  FROM worker_instances
		 WHERE worker_instances.id = $3
	`, ids.runID, ids.orgID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	workspaceMountID := uuid.Must(uuid.NewV7())
	seedResidentRuntimeWorkspaceMount(t, ctx, pool, ids, ids.workspaceID, workspaceMountID, workerID, 1000, 1024, 4096, 1)
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'checkpointing',
		       checkpointing_at = now()
		 WHERE org_id = $1
		   AND workspace_mount_id = $2
	`, ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: dispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-checkpointing-resident",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "5555555555555555",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseRunLease checkpointing resident error = %v, want pgx.ErrNoRows", err)
	}
	var state db.RuntimeInstanceState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND workspace_mount_id = $2
	`, ids.orgID, workspaceMountID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.RuntimeInstanceStateCheckpointing {
		t.Fatalf("runtime state after rejected lease = %s, want checkpointing", state)
	}
}

func seedWrongWorkerGroupRuntimeSubstrateArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, ids integrationIDs) {
	t.Helper()
	otherWorkerGroupID, otherSandboxID := seedRuntimeSubstrateSourceInOtherWorkerGroup(t, ctx, pool, ids, "run-lease-wrong-worker-group-runtime-substrate")
	digest := testDigest("run-lease-wrong-worker-group-runtime-substrate")
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		Digest:    digest,
		SizeBytes: 1024,
		MediaType: "application/vnd.helmr.runtime-substrate.v0.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	artifact, err := queries.UpsertRuntimeSubstrateArtifactBlob(ctx, db.UpsertRuntimeSubstrateArtifactBlobParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Digest:        digest,
		SizeBytes:     1024,
		MediaType:     "application/vnd.helmr.runtime-substrate.v0.ext4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRuntimeSubstrateArtifact(ctx, db.UpsertRuntimeSubstrateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             otherWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(otherSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:run-lease-wrong-worker-group-runtime-substrate",
		SubstrateFormat:           "ext4",
		BuilderAbi:                "builder-v0",
		LayoutAbi:                 "layout-v0",
		SubstrateSizeBytes:        1024,
		Source:                    []byte(`{"test":"run-lease-wrong-worker-group-runtime-substrate"}`),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
}

func seedRuntimePressureWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, cpu int, memory int, disk int64, slots int) uuid.UUID {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE id = $1 AND name = 'default'`, dbtest.DefaultWorkerGroupID).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
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
		INSERT INTO worker_instances (
			id, org_id, worker_group_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6,
			$7, $8, $9, $10, $7, $8, $9, $10,
			$11, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultWorkerGroupID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, cpu, memory, disk, slots, runtimeID); err != nil {
		t.Fatal(err)
	}
	return workerID
}

func seedResidentRuntimeWorkspaceMount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, workspaceID uuid.UUID, workspaceMountID uuid.UUID, workerID uuid.UUID, cpu int, memory int, disk int64, slots int) {
	t.Helper()
	runtimeInstanceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		WITH mounted AS (
				INSERT INTO workspace_mounts (
					id, org_id, worker_group_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
				base_version_id,
				image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
				workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
				workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type,
				workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state, mounted_at
			)
				SELECT $1, workspaces.org_id, workspaces.worker_group_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
			       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
			       workspaces.current_version_id,
			       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
			       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
			       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
			       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
			       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
			       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted', now()
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
				 WHERE workspaces.org_id = $2
				   AND workspaces.id = $3
			RETURNING *
		),
		runtime AS (
				INSERT INTO runtime_instances (
					id, org_id, worker_group_id, project_id, environment_id, worker_instance_id, runtime_release_id,
				deployment_sandbox_id, runtime_key_hash, runtime_key, sandbox_fingerprint,
				rootfs_digest, image_digest, image_format, sandbox_image_artifact_id,
				sandbox_image_artifact_digest, sandbox_image_artifact_format, workspace_mount_path,
				runtime_abi, guestd_abi, adapter_abi, reserved_cpu_millis, reserved_memory_mib,
				reserved_disk_mib, reserved_execution_slots, workspace_mount_id, owner_workspace_id,
				owner_workspace_version_id, state, instance_token, last_heartbeat_at, running_at
			)
					SELECT $4, mounted.org_id, mounted.worker_group_id, mounted.project_id, mounted.environment_id, $5, worker_instances.runtime_id,
				       mounted.deployment_sandbox_id, 'resident-runtime-' || ($4::uuid)::text, '{}'::jsonb, mounted.sandbox_fingerprint,
				       mounted.rootfs_digest, mounted.image_digest, mounted.image_format, mounted.image_artifact_id,
				       mounted.image_digest, mounted.image_artifact_format, mounted.workspace_mount_path,
				       mounted.runtime_abi, mounted.guestd_abi, mounted.adapter_abi, $6, $7, $8, $9,
				       mounted.id, mounted.workspace_id, mounted.base_version_id, 'running', 'resident-runtime-token-' || ($4::uuid)::text,
				       now(), now()
				  FROM mounted
				  JOIN worker_instances ON worker_instances.id = $5
			RETURNING id
		)
		UPDATE workspace_mounts
		   SET runtime_instance_id = runtime.id,
		       updated_at = now()
		  FROM runtime
			 WHERE workspace_mounts.org_id = $2
			   AND workspace_mounts.id = $1
		`, workspaceMountID, ids.orgID, workspaceID, runtimeInstanceID, workerID, cpu, memory, disk, slots); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET runtime_instance_id = $1,
		       updated_at = now()
		 WHERE org_id = $2
		   AND id = $3
	`, runtimeInstanceID, ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
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
		OrgID:                    pgvalue.UUID(ids.orgID),
		RunID:                    pgvalue.UUID(ids.runID),
		RunLeaseID:               pgvalue.UUID(runLeaseID),
		WorkerInstanceID:         pgvalue.UUID(workerID),
		DispatchMessageID:        "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:          "lease-" + runLeaseID.String()[:8],
		RunStatus:                db.RunStatusFailed,
		AttemptStatus:            db.RunAttemptStatusFailed,
		ExitCode:                 pgtype.Int4{},
		ErrorMessage:             pgtype.Text{String: "payload build failed", Valid: true},
		TerminalEventKind:        "run.failed",
		TerminalEventPayload:     []byte(`{"status":"failed"}`),
		WorkspaceVersionPublicID: testWorkspaceVersionPublicID(t),
		WorkspaceFencingToken:    pgtype.Text{},
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

func TestGetRunLeaseQueueLeaseRejectsDisabledSourceRoute(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)

	_, err := queries.GetRunLeaseQueueLease(ctx, db.GetRunLeaseQueueLeaseParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetRunLeaseQueueLease disabled route error = %v, want pgx.ErrNoRows", err)
	}
}

func TestRenewRunLeaseAllowsStaleWorkerGroupHealthForInFlightLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	renewed, err := queries.RenewRunLease(ctx, db.RenewRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:   "lease-" + runLeaseID.String()[:8],
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pgvalue.MustUUIDValue(renewed.ID); got != runLeaseID {
		t.Fatalf("renewed lease id = %s, want %s", got, runLeaseID)
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
		OrgID:                    pgvalue.UUID(ids.orgID),
		RunID:                    pgvalue.UUID(ids.runID),
		RunLeaseID:               pgvalue.UUID(runLeaseID),
		WorkerInstanceID:         pgvalue.UUID(workerID),
		DispatchMessageID:        "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:          "lease-" + runLeaseID.String()[:8],
		RunStatus:                db.RunStatusFailed,
		AttemptStatus:            db.RunAttemptStatusFailed,
		ExitCode:                 pgtype.Int4{},
		ErrorMessage:             pgtype.Text{String: "clock skew regression", Valid: true},
		TerminalEventKind:        "run.failed",
		TerminalEventPayload:     []byte(`{"status":"failed"}`),
		WorkspaceVersionPublicID: testWorkspaceVersionPublicID(t),
		WorkspaceFencingToken:    pgtype.Text{},
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
