package runtest

import (
	"context"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Region      = "us-east-1"
	WorkerGroup = "run-workers"
)

type Fixture struct {
	Pool                  *pgxpool.Pool
	OrgID                 uuid.UUID
	ProjectID             uuid.UUID
	EnvironmentID         uuid.UUID
	DeploymentID          uuid.UUID
	TaskDefinitionID      uuid.UUID
	WorkspaceDefinitionID uuid.UUID
	WorkerID              uuid.UUID
	WorkerPoolID          uuid.UUID
	RuntimeIdentityID     string
	CPUConfigDigest       string
}

type RunLease struct {
	LeaseID uuid.UUID
	RunID   uuid.UUID
}

func New(t *testing.T) Fixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	fixture := Fixture{
		Pool:                  database.Pool,
		OrgID:                 uuid.NewV7(),
		ProjectID:             uuid.NewV7(),
		EnvironmentID:         uuid.NewV7(),
		DeploymentID:          uuid.NewV7(),
		TaskDefinitionID:      uuid.NewV7(),
		WorkspaceDefinitionID: uuid.NewV7(),
		WorkerID:              uuid.NewV7(),
		WorkerPoolID:          uuid.NewV7(),
		RuntimeIdentityID:     dbtest.Digest("run-lease-test-runtime"),
		CPUConfigDigest:       dbtest.Digest("run-lease-test-cpu-config"),
	}
	programID := uuid.NewV7()
	imageID := uuid.NewV7()
	bundleDigest := dbtest.Digest("bundle")
	runtimeArtifactDigest := dbtest.Digest("runtime-artifact")
	programDigest := dbtest.Digest("program")
	imageDigest := dbtest.Digest("image")
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO regions (id, display_name)
		VALUES ($1, 'Run Lease Test')
	`, Region)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		WITH token AS (
			INSERT INTO worker_group_tokens (id, token_hash)
			VALUES ($3, $4)
			RETURNING id
		)
		INSERT INTO worker_groups (
			id, token_id, region_id, name
		)
		SELECT $1, token.id, $2, $1 FROM token
	`, WorkerGroup, Region, uuid.NewV7(), dbtest.Hash("run-test-worker-group"))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Run Lease Test', $2)
	`, fixture.OrgID, "run-lease-"+dbtest.ShortID(fixture.OrgID))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Run Lease Test')
	`, fixture.ProjectID, fixture.OrgID, Region, "run-lease-"+dbtest.ShortID(fixture.ProjectID))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Run Lease Test', '#3366ff')
	`, fixture.EnvironmentID, fixture.OrgID, fixture.ProjectID,
		"run-lease-"+dbtest.ShortID(fixture.EnvironmentID))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES
			($1, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($1, $3, 1, 'application/octet-stream')
	`, fixture.OrgID, programDigest, imageDigest)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES
			($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($2, $3, $4, $5, $7, 'workspace_image', 1, 'application/octet-stream')
	`, programID, imageID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, programDigest, imageDigest)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployments (
			id, org_id, project_id, environment_id, version, bundle_digest,
			runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
			$1, $2, $3, $4, 'run-lease-test', $5, $6, $7,
			decode(repeat('03', 32), 'hex'), '{}'::jsonb
		)
	`, fixture.DeploymentID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, bundleDigest, runtimeArtifactDigest, programID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		) VALUES (
			$1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), NULL
		), (
			$2, $3, $4, 'sandbox', 'test-workspace', 0, '{}'::jsonb,
			decode(repeat('04', 32), 'hex'), $5
		)
	`, fixture.TaskDefinitionID, fixture.WorkspaceDefinitionID,
		fixture.EnvironmentID, fixture.DeploymentID, imageID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, vm_runtime_contract, vm_runtime_descriptor_digest,
			firecracker_digest, firecracker_version, snapshot_format_version,
			host_kernel_release, cpu_template_kind,
			kernel_digest, initramfs_digest, rootfs_digest
		) VALUES (
			$1, 'x86_64', 'helmr.vm-runtime.v0', $2,
			$3, '1.16.1', '6.0.0', '6.8.0-test', 'none',
			$4, $5, $6
		)
	`, fixture.RuntimeIdentityID, dbtest.Digest("run-lease-vm-runtime-descriptor"),
		dbtest.Digest("run-lease-firecracker"), dbtest.Digest("run-lease-kernel"),
		dbtest.Digest("run-lease-initramfs"), dbtest.Digest("run-lease-rootfs"))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_pools (
			id, worker_group_id, name, state,
			runtime_identity_id, substrate_format, substrate_contract,
			capacity_cpu_millis, capacity_memory_bytes, capacity_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, sealed_at
		) VALUES (
			$1, $2, 'default', 'active',
			$3, 'squashfs', 'builder-v0',
			8000, 8589934592, 17179869184,
			1000, 1073741824, 2147483648,
			8, now()
		)
	`, fixture.WorkerPoolID, WorkerGroup, fixture.RuntimeIdentityID)
	for vcpu := int32(1); vcpu <= 1; vcpu++ {
		dbtest.MustExec(t, t.Context(), fixture.Pool, `
			INSERT INTO worker_pool_cpu_shapes (worker_pool_id, vcpu_count, cpu_config_digest)
			VALUES ($1, $2, $3)
		`, fixture.WorkerPoolID, vcpu, fixture.CPUConfigDigest)
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		UPDATE worker_groups SET primary_pool_id = $2 WHERE id = $1
	`, WorkerGroup, fixture.WorkerPoolID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, worker_pool_id, state,
			current_epoch, current_service_id,
			runtime_identity_id,
			substrate_format, substrate_contract,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_runtime_starts,
			cpu_environment, cpu_environment_digest,
			observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active', 1, $5,
			$6, 'squashfs', 'builder-v0',
			8000, 8589934592, 17179869184,
			1000, 1073741824, 2147483648,
			8, 8, '{}'::jsonb, $7, now(), now(), now()
		)
	`, fixture.WorkerID, fixture.WorkerID.String(), WorkerGroup,
		fixture.WorkerPoolID, uuid.NewV7(), fixture.RuntimeIdentityID,
		fixture.CPUConfigDigest)
	return fixture
}

func (fixture Fixture) AddRunLease(t *testing.T, state string, assignedAt time.Time) RunLease {
	t.Helper()
	ctx := t.Context()
	workspaceID := uuid.NewV7()
	versionID := uuid.NewV7()
	runID := uuid.NewV7()
	runtimeID := uuid.NewV7()
	mountID := uuid.NewV7()
	leaseID := uuid.NewV7()
	workspaceLeaseID := uuid.NewV7()
	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspaces (
			id, environment_id, region_id,
			sandbox_declared_id, deployment_definition_id,
			owner_run_id, ownership_generation, writer_generation, head_version_id
		) VALUES (
			$1, $2, $3, 'test-workspace', $4,
			$5, 1, 1, $6
		)
	`, workspaceID, fixture.EnvironmentID, Region,
		fixture.WorkspaceDefinitionID, runID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_versions (
			id, environment_id, workspace_id,
			kind, content_digest, state, ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, 'system',
			'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			'committed', 0, 0, now()
		)
	`, versionID, fixture.EnvironmentID, workspaceID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, workspace_id, base_workspace_version_id, payload,
			queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
			retry_policy, trace_id, root_span_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'task', 'test-task', 'api',
			$7, $8, '{}'::jsonb, 'default', now(), now(), 300000,
			'{"enabled":false}'::jsonb,
			'11111111111111111111111111111111', '2222222222222222'
		)
	`, runID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, fixture.DeploymentID, fixture.TaskDefinitionID,
		workspaceID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
		) VALUES ($1, 1, 'task', $2, $3)
	`, runID, workspaceID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_definition_id,
			worker_epoch, vm_vcpu_count, cpu_config_digest,
			reserved_cpu_millis, reserved_memory_bytes,
			reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
			workspace_id, program_deployment_id, desired_reason, observed_state,
			observed_version, observed_desired_version, preparing_at, ready_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 1, $12,
			1000, 1073741824, 2147483648, 1,
			$10, $11, 'test', 'ready', 1, 1, now(), now()
		)
	`, runtimeID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID,
		fixture.RuntimeIdentityID, fixture.WorkspaceDefinitionID, workspaceID,
		fixture.DeploymentID, fixture.CPUConfigDigest)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
			runtime_instance_id, state, fencing_generation, mounted_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, 'mounted', 2, now()
		)
	`, mountID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID, workspaceID,
		versionID, runtimeID)
	var claimedAt any
	if state == "starting" {
		claimedAt = assignedAt.Add(time.Second)
	}
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id,
			runtime_identity_id, requested_cpu_millis,
			requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, 1, $8, $9, 1, $10,
			$11, 1000, 1073741824, 2147483648, 1,
			$12::text, $13, now() + interval '5 minutes', $14,
			now() + interval '10 minutes'
		)
	`, leaseID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, runID,
		workspaceID, Region, WorkerGroup, fixture.WorkerID,
		runtimeID, fixture.RuntimeIdentityID,
		state, assignedAt, claimedAt)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_token_hash, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, $11, $12,
			1, 1, 2, 'test-token-hash', now() + interval '10 minutes'
		)
	`, workspaceLeaseID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID, runtimeID,
		workspaceID, mountID, leaseID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1, first_lease_at = $2
		 WHERE id = $3
	`, leaseID, assignedAt, runID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return RunLease{LeaseID: leaseID, RunID: runID}
}

func (fixture Fixture) ConvertToActor(
	t *testing.T,
	ctx context.Context,
	work RunLease,
	retryPolicy string,
) uuid.UUID {
	t.Helper()
	actorDefinitionID := uuid.NewV7()
	actorID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.Pool, `
ALTER TABLE run_attempts
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_workspace_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, work.RunID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest
) VALUES (
    $1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb,
    decode(repeat('05', 32), 'hex')
)`, actorDefinitionID, fixture.EnvironmentID, fixture.DeploymentID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO sessions (
    id, environment_id,
    actor_declared_id, deployment_definition_id, workspace_id, current_run_id,
    next_input_sequence, committed_input_sequence,
    run_queue_name, run_max_active_duration_ms, run_retry_policy
) VALUES (
    $1, $2,
    'test-actor', $3, $4, $5,
    3, 1, 'default', 300000, $6::jsonb
)`, actorID, fixture.EnvironmentID, actorDefinitionID, workspaceID, work.RunID, retryPolicy)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspaces
   SET owner_session_id = $1, owner_run_id = NULL
 WHERE id = $2`, actorID, workspaceID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $1,
       entrypoint_kind = 'actor', entrypoint_declared_id = 'test-actor',
       session_id = $2, cause_kind = 'actor_start',
       session_input_start_sequence = 1, session_input_high_watermark = 2,
       payload = NULL, retry_policy = $3::jsonb
 WHERE id = $4`, actorDefinitionID, actorID, retryPolicy, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor',
       session_input_start_sequence = 1
 WHERE run_id = $1 AND number = 1`, work.RunID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return actorID
}
