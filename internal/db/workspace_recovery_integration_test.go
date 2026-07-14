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
	"github.com/jackc/pgx/v5/pgxpool"
)

type workspaceRuntimeFixture struct {
	workerID  uuid.UUID
	runtimeID uuid.UUID
	mountID   uuid.UUID
	slotID    uuid.UUID
}

func seedWorkspaceRuntimeFixture(t *testing.T, ctx context.Context, ids integrationIDs, mounted bool, pool *pgxpool.Pool) workspaceRuntimeFixture {
	t.Helper()

	fixture := workspaceRuntimeFixture{
		workerID:  uuid.Must(uuid.NewV7()),
		runtimeID: uuid.Must(uuid.NewV7()),
		mountID:   uuid.Must(uuid.NewV7()),
		slotID:    uuid.Must(uuid.NewV7()),
	}
	serviceID := uuid.Must(uuid.NewV7())
	groupID := "workspace-recovery-" + shortUUID(fixture.workerID)

	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'registering', 1, $4, now())
	`, fixture.workerID, fixture.workerID.String(), groupID, serviceID)
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_sandbox_id, worker_epoch,
			runtime_key_hash, runtime_key, sandbox_fingerprint, rootfs_digest,
			image_digest, image_format, sandbox_image_artifact_id,
			sandbox_image_artifact_digest, sandbox_image_artifact_format,
			runtime_abi, guestd_abi, adapter_abi, network_policy,
			reserved_cpu_millis, reserved_memory_bytes, reserved_workload_disk_bytes,
			reserved_scratch_bytes, reserved_execution_slots, desired_reason,
			observed_state, observed_version, observed_desired_version,
			preparing_at, ready_at, allocated_at
		)
		SELECT $1, sandboxes.org_id, $2, sandboxes.project_id, sandboxes.environment_id, $3,
		       $4, 'test-runtime', sandboxes.id, 1,
		       $5, '{}', sandboxes.fingerprint, sandboxes.rootfs_digest,
		       sandboxes.image_digest, sandboxes.image_format, sandboxes.image_artifact_id,
		       artifacts.digest, sandboxes.image_artifact_format,
		       sandboxes.runtime_abi, sandboxes.guestd_abi, sandboxes.adapter_abi, '{}',
		       1000, 1073741824, 1073741824, 0, 1, 'workspace-recovery-test',
		       'ready', 1, 1, now(), now(), now()
		  FROM deployment_sandboxes AS sandboxes
		  JOIN artifacts ON artifacts.org_id = sandboxes.org_id
		                AND artifacts.project_id = sandboxes.project_id
		                AND artifacts.environment_id = sandboxes.environment_id
		                AND artifacts.id = sandboxes.image_artifact_id
		 WHERE sandboxes.id = $6
	`, fixture.runtimeID, groupID, dbtest.DefaultRegionID, fixture.workerID,
		"workspace-runtime-"+shortUUID(fixture.runtimeID), ids.deploymentSandboxID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, runtime_instance_id, host_interface_name,
			guest_address, gateway_address, subnet, tap_name, netns_name, guest_mac, assigned_at
		) VALUES (
			$1, $2, $3, 1, 'vm-0001', 1, 'bound', $4, 'veth-test',
			'10.0.0.2', '10.0.0.1', '10.0.0.0/30', 'tap-test', 'netns-test',
			'02:00:00:00:00:01', now()
		)
	`, fixture.slotID, groupID, fixture.workerID, fixture.runtimeID)

	mountState := "unmounting"
	if mounted {
		mountState = "mounted"
	}
	mustExec(t, ctx, pool, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, workspace_id, deployment_sandbox_id,
			sandbox_fingerprint, base_version_id, runtime_instance_id, state,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
			workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, mounted_at, stopped_at
		)
		SELECT $1, workspaces.org_id, $2, workspaces.project_id, workspaces.environment_id, $3,
		       $4, 1, workspaces.id, sandboxes.id, sandboxes.fingerprint,
		       versions.id, $5, $6::workspace_mount_state,
		       sandboxes.image_artifact_id, sandboxes.image_artifact_format,
		       sandboxes.rootfs_digest, sandboxes.image_digest, sandboxes.image_format,
		       versions.artifact_id, versions.artifact_encoding, versions.artifact_entry_count,
		       versions.content_digest, versions.size_bytes, workspace_artifacts.media_type,
		       sandboxes.workspace_mount_path, sandboxes.runtime_abi, sandboxes.guestd_abi,
		       sandboxes.adapter_abi, now(), CASE WHEN $6::text = 'unmounting' THEN now() END
		  FROM workspaces
		  JOIN workspace_versions AS versions
		    ON versions.org_id = workspaces.org_id
		   AND versions.workspace_id = workspaces.id
		   AND versions.id = workspaces.current_version_id
		  JOIN artifacts AS workspace_artifacts
		    ON workspace_artifacts.org_id = versions.org_id
		   AND workspace_artifacts.id = versions.artifact_id
		  JOIN deployment_sandboxes AS sandboxes
		    ON sandboxes.org_id = workspaces.org_id
		   AND sandboxes.id = workspaces.deployment_sandbox_id
		 WHERE workspaces.id = $7
	`, fixture.mountID, groupID, dbtest.DefaultRegionID, fixture.workerID,
		fixture.runtimeID, mountState, ids.workspaceID)

	return fixture
}

func activateWorkspaceWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID) {
	t.Helper()
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'active', supports_run = true, runtime_identity_id = 'test-runtime',
		       certified_cpu_millis = 4000, certified_memory_bytes = 8589934592,
		       certified_workload_disk_bytes = 10737418240, certified_scratch_bytes = 10737418240,
		       per_vm_cpu_millis = 2000, per_vm_memory_bytes = 2147483648,
		       per_vm_workload_disk_bytes = 4294967296, per_vm_scratch_bytes = 4294967296,
		       max_vm_slots = 2, max_run_consumers = 2, max_runtime_starts = 2,
		       certification_profile = 'test', certification_fingerprint = 'test-fingerprint',
		       certified_at = now(), activated_at = now()
		 WHERE id = $1
	`, workerID)
}

func TestWorkspaceOperationClaimSerializesBeforeWorkerDrain(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	activateWorkspaceWorker(t, ctx, pool, fixture.workerID)
	processID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	operationID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO workspace_processes (
			id, org_id, project_id, environment_id, workspace_id, kind, command
		) VALUES ($1, $2, $3, $4, $5, 'exec', '["true"]'::jsonb)
	`, processID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID)
	mustExec(t, ctx, pool, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, lease_kind, owner_process_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, workspace_mounts.org_id, workspace_mounts.worker_group_id,
		       workspace_mounts.project_id, workspace_mounts.environment_id, workspace_mounts.region_id,
		       workspace_mounts.worker_instance_id, workspace_mounts.worker_epoch,
		       workspace_mounts.runtime_instance_id, workspace_mounts.workspace_id,
		       workspace_mounts.id, 'instance', $2,
		       workspace_mounts.fencing_generation, 'claim-race-fence', now() + interval '5 minutes'
		  FROM workspace_mounts WHERE workspace_mounts.id = $3
	`, leaseID, processID, fixture.mountID)
	mustExec(t, ctx, pool, `
		INSERT INTO workspace_process_operations (
			id, org_id, project_id, environment_id, workspace_id, workspace_mount_id,
			operation_kind, process_id, request_fingerprint, operation_expires_at,
			instance_lease_id, fencing_token, fencing_generation, request
		)
		SELECT $1, workspace_mounts.org_id, workspace_mounts.project_id,
		       workspace_mounts.environment_id, workspace_mounts.workspace_id, workspace_mounts.id,
		       'start_process', $2, 'claim-race', now() + interval '5 minutes',
		       $3, 'claim-race-fence', workspace_mounts.fencing_generation, '{}'::jsonb
		  FROM workspace_mounts WHERE workspace_mounts.id = $4
	`, operationID, processID, leaseID, fixture.mountID)

	claimTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claimTx.Rollback(context.Background()) }()
	claimed, err := db.New(claimTx).ClaimWorkspaceOperation(ctx, db.ClaimWorkspaceOperationParams{
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: pgtype.Int8{Int64: 1, Valid: true},
		OrgID: pgvalue.UUID(ids.orgID), WorkspaceMountID: pgvalue.UUID(fixture.mountID),
		ClaimToken: "claim-race-token", ClaimExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != pgvalue.UUID(operationID) {
		t.Fatalf("claimed operation = %v, want %s", claimed.ID, operationID)
	}

	groupID := claimedWorkerGroup(t, ctx, pool, fixture.workerID)
	drainResult := make(chan error, 1)
	go func() {
		_, err := db.New(pool).MarkFleetWorkerDraining(ctx, db.MarkFleetWorkerDrainingParams{
			WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerGroupID: groupID, WorkerRole: "run",
		})
		drainResult <- err
	}()
	select {
	case err := <-drainResult:
		t.Fatalf("worker drain overtook uncommitted operation claim: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := claimTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-drainResult; err != nil {
		t.Fatal(err)
	}
	var workerState db.WorkerInstanceState
	var operationState db.WorkspaceOperationState
	if err := pool.QueryRow(ctx, `
		SELECT worker_instances.state, workspace_process_operations.state
		  FROM worker_instances, workspace_process_operations
		 WHERE worker_instances.id = $1 AND workspace_process_operations.id = $2
	`, fixture.workerID, operationID).Scan(&workerState, &operationState); err != nil {
		t.Fatal(err)
	}
	if workerState != db.WorkerInstanceStateDraining || operationState != db.WorkspaceOperationStateClaimed {
		t.Fatalf("worker=%s operation=%s", workerState, operationState)
	}
}

func claimedWorkerGroup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID) string {
	t.Helper()
	var groupID string
	if err := pool.QueryRow(ctx, `SELECT worker_group_id FROM worker_instances WHERE id = $1`, workerID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	return groupID
}

func TestStopWorkspaceMountClosesRuntimeAndReclaimsNetworkSlot(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, false, pool)
	queries := db.New(pool)

	stopped, err := queries.StopWorkspaceMount(ctx, db.StopWorkspaceMountParams{
		ReasonCode:        pgtype.Text{String: "run_released", Valid: true},
		OrgID:             pgvalue.UUID(ids.orgID),
		ID:                pgvalue.UUID(fixture.mountID),
		WorkerInstanceID:  pgvalue.UUID(fixture.workerID),
		WorkerEpoch:       1,
		RuntimeInstanceID: pgvalue.UUID(fixture.runtimeID),
		FencingGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != db.WorkspaceMountStateUnmounted {
		t.Fatalf("mount state = %s, want unmounted", stopped.State)
	}

	var runtimeState db.RuntimeObservedState
	var desiredState db.RuntimeDesiredState
	var reclaimedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT observed_state, desired_state, reclaimed_at
		  FROM runtime_instances WHERE id = $1
	`, fixture.runtimeID).Scan(&runtimeState, &desiredState, &reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if runtimeState != db.RuntimeObservedStateClosed || desiredState != db.RuntimeDesiredStateClosed || !reclaimedAt.Valid {
		t.Fatalf("runtime = %s/%s reclaimed=%v, want closed/closed and reclaimed", runtimeState, desiredState, reclaimedAt.Valid)
	}

	var slotState db.WorkerNetworkSlotState
	var slotRuntimeID pgtype.UUID
	var generation int64
	if err := pool.QueryRow(ctx, `
		SELECT state, runtime_instance_id, generation
		  FROM worker_network_slots WHERE id = $1
	`, fixture.slotID).Scan(&slotState, &slotRuntimeID, &generation); err != nil {
		t.Fatal(err)
	}
	if slotState != db.WorkerNetworkSlotStateAvailable || slotRuntimeID.Valid || generation != 2 {
		t.Fatalf("slot = %s runtime_valid=%v generation=%d, want available/false/2", slotState, slotRuntimeID.Valid, generation)
	}
}

func TestDrainWorkerRequestsIdleMountedRuntimeCleanup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	queries := db.New(pool)

	activateWorkspaceWorker(t, ctx, pool, fixture.workerID)

	var groupID string
	if err := pool.QueryRow(ctx, `SELECT worker_group_id FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
		ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: groupID,
		ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	var workerState db.WorkerInstanceState
	var mountState db.WorkspaceMountState
	var desiredState db.RuntimeDesiredState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&workerState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM workspace_mounts WHERE id = $1`, fixture.mountID).Scan(&mountState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM runtime_instances WHERE id = $1`, fixture.runtimeID).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if workerState != db.WorkerInstanceStateDraining || mountState != db.WorkspaceMountStateUnmounting || desiredState != db.RuntimeDesiredStateReady {
		t.Fatalf("drain state = worker:%s mount:%s runtime:%s, want draining/unmounting/ready until capture", workerState, mountState, desiredState)
	}
}

func TestFenceWorkerDurablyInvalidatesMountedRuntimeAuthority(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	queries := db.New(pool)
	activateWorkspaceWorker(t, ctx, pool, fixture.workerID)

	var groupID string
	if err := pool.QueryRow(ctx, `SELECT worker_group_id FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.FenceWorkerInstance(ctx, db.FenceWorkerInstanceParams{
		ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: groupID,
		ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true},
		ReasonCode:    pgtype.Text{String: "termination_drain_failed", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	var workerState db.WorkerInstanceState
	var mountState db.WorkspaceMountState
	var runtimeState db.RuntimeObservedState
	var slotState db.WorkerNetworkSlotState
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&workerState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM workspace_mounts WHERE id = $1`, fixture.mountID).Scan(&mountState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT observed_state FROM runtime_instances WHERE id = $1`, fixture.runtimeID).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM worker_network_slots WHERE id = $1`, fixture.slotID).Scan(&slotState); err != nil {
		t.Fatal(err)
	}
	if workerState != db.WorkerInstanceStateLost || mountState != db.WorkspaceMountStateLost || runtimeState != db.RuntimeObservedStateLost || slotState != db.WorkerNetworkSlotStateLost {
		t.Fatalf("fenced state = worker:%s mount:%s runtime:%s slot:%s, want all lost", workerState, mountState, runtimeState, slotState)
	}
	if _, err := queries.GetNextRuntimeReconcileTarget(ctx, db.GetNextRuntimeReconcileTargetParams{
		WorkerGroupID: groupID, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fenced worker reconcile error = %v, want pgx.ErrNoRows", err)
	}
}

func TestExpiredWorkspaceWriteLeaseFencesMountAndRequestsRuntimeClose(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	queries := db.New(pool)

	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerRunID:       pgvalue.UUID(ids.runID),
		FencingToken:     "expired-write-lease",
		ExpiresAt:        pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkspaceID:      pgvalue.UUID(ids.workspaceID),
		WorkspaceMountID: pgvalue.UUID(fixture.mountID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.AcquiredFencingGeneration != 2 {
		t.Fatalf("acquired fencing generation = %d, want 2", lease.AcquiredFencingGeneration)
	}

	expired, err := queries.ExpireWorkspaceLeases(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != lease.ID || expired[0].State != db.WorkspaceLeaseStateExpired {
		t.Fatalf("expired leases = %+v, want acquired lease expired", expired)
	}

	var mountState db.WorkspaceMountState
	var fencingGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT state, fencing_generation FROM workspace_mounts WHERE id = $1
	`, fixture.mountID).Scan(&mountState, &fencingGeneration); err != nil {
		t.Fatal(err)
	}
	if mountState != db.WorkspaceMountStateUnmounting || fencingGeneration != 3 {
		t.Fatalf("mount = %s generation=%d, want unmounting/3", mountState, fencingGeneration)
	}

	var desiredState db.RuntimeDesiredState
	var desiredVersion int64
	var observedState db.RuntimeObservedState
	if err := pool.QueryRow(ctx, `
		SELECT desired_state, desired_version, observed_state
		  FROM runtime_instances WHERE id = $1
	`, fixture.runtimeID).Scan(&desiredState, &desiredVersion, &observedState); err != nil {
		t.Fatal(err)
	}
	if desiredState != db.RuntimeDesiredStateClosed || desiredVersion != 2 || observedState != db.RuntimeObservedStateReady {
		t.Fatalf("runtime = desired %s/%d observed %s, want closed/2 and ready", desiredState, desiredVersion, observedState)
	}
}
