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

func TestTaskSessionLoserRunIsNotVisibleOrLeaseable(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	taskSessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	workspace, err := queries.CreateTaskSessionWorkspace(ctx, db.CreateTaskSessionWorkspaceParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		TaskSessionID:   pgvalue.UUID(taskSessionID),
		RetentionPolicy: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := pgvalue.MustUUIDValue(workspace.ID)
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
			artifact_encoding, artifact_entry_count, mount_path, volume_kind, state
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'tar', 1, '/workspace', 'copy-on-write', 'active')
	`, baseVersionID, ids.orgID, ids.projectID, ids.environmentID, workspaceID, baseArtifactID); err != nil {
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
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id,
			task_session_id, status, execution_status, payload, queue_name, queue_timestamp,
			max_duration_seconds, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval-task',
			$7, 'queued', 'queued', '{}', 'default', now(), 300,
			'11111111111111111111111111111111', '2222222222222222')
	`, loserRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, taskSessionID); err != nil {
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
		UPDATE workspace_versions
		   SET state = 'superseded'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, baseVersionID); err != nil {
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
		RunLeaseSpanID:    "5555555555555555",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseRunLease with superseded current workspace version error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_versions
		   SET state = 'active'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, baseVersionID); err != nil {
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
		WorkspaceArtifactDigest:     pgvalue.Text(workspaceDigest),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:         pgvalue.Text("copy-on-write"),
		WorkspaceBaseVersionID:      pgvalue.UUID(uuid.Must(uuid.NewV7())),
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      []byte(`{"ok":true}`),
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{"status":"succeeded"}`),
		ReleaseActiveDurationMs:     1,
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
		WorkspaceArtifactDigest:     pgvalue.Text(workspaceDigest),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		WorkspaceVolumeKind:         pgvalue.Text("copy-on-write"),
		WorkspaceBaseVersionID:      leasedWinner.WorkspaceBaseVersionID,
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      []byte(`{"ok":true}`),
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{"status":"succeeded"}`),
		ReleaseActiveDurationMs:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusSucceeded {
		t.Fatalf("released status = %s, want succeeded", released.Status)
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
