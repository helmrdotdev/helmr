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
	Region          = "us-east-1"
	WorkerGroup     = "01900000-0000-7000-8000-000000000101"
	WorkerGroupName = "run-workers"
)

var WorkerGroupID = uuid.MustParse(WorkerGroup)

type Fixture struct {
	Pool                 *pgxpool.Pool
	OrgID                uuid.UUID
	ProjectID            uuid.UUID
	EnvironmentID        uuid.UUID
	DeploymentID         uuid.UUID
	TaskDefinitionID     uuid.UUID
	ComputerDefinitionID uuid.UUID
	WorkerID             uuid.UUID
	WorkerPoolID         uuid.UUID
	VMPlatformID         string
	CPUConfigDigest      string
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
		Pool:                 database.Pool,
		OrgID:                uuid.NewV7(),
		ProjectID:            uuid.NewV7(),
		EnvironmentID:        uuid.NewV7(),
		DeploymentID:         uuid.NewV7(),
		TaskDefinitionID:     uuid.NewV7(),
		ComputerDefinitionID: uuid.NewV7(),
		WorkerID:             uuid.NewV7(),
		WorkerPoolID:         uuid.NewV7(),
		VMPlatformID:         dbtest.Digest("run-lease-test-runtime"),
		CPUConfigDigest:      dbtest.Digest("run-lease-test-cpu-config"),
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
		SELECT $1, token.id, $2, $5 FROM token
	`, WorkerGroup, Region, uuid.NewV7(), dbtest.Hash("run-test-worker-group"), WorkerGroupName)
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
		WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1), ($3, 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES
			($1, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($1, $3, 1, 'application/vnd.helmr.computer.seed.v0+filepack')
	`, fixture.OrgID, programDigest, imageDigest)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES
			($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($2, $3, $4, $5, $7, 'computer_image', 1, 'application/vnd.helmr.computer.seed.v0+filepack')
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
			manifest_version, manifest, manifest_digest, computer_spec_id
		) VALUES (
			$1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), NULL
		), (
			$2, $3, $4, 'sandbox', 'test-computer', 0, '{}'::jsonb,
			decode(repeat('04', 32), 'hex'), $5
		)
	`, fixture.TaskDefinitionID, fixture.ComputerDefinitionID,
		fixture.EnvironmentID, fixture.DeploymentID, dbtest.InsertDefaultComputerSpec(t, t.Context(), fixture.Pool, imageID))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO vm_platforms (
			id, arch, contract, descriptor_digest,
			firecracker_digest, firecracker_version, snapshot_format_version,
			host_kernel_release, cpu_template_kind,
			kernel_digest, initramfs_digest, rootfs_digest
		) VALUES (
			$1, 'x86_64', 'helmr.vm-runtime.v0', $2,
			$3, '1.16.1', '6.0.0', '6.8.0-test', 'none',
			$4, $5, $6
		)
	`, fixture.VMPlatformID, dbtest.Digest("run-lease-vm-runtime-descriptor"),
		dbtest.Digest("run-lease-firecracker"), dbtest.Digest("run-lease-kernel"),
		dbtest.Digest("run-lease-initramfs"), dbtest.Digest("run-lease-rootfs"))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_pools (
			id, worker_group_id, name, status,
			vm_platform_id,
			capacity_cpu_millis, capacity_memory_bytes, capacity_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, sealed_at
		) VALUES (
			$1, $2, 'default', 'active',
			$3,
			8000, 8589934592, 17179869184,
			1000, 1073741824, 2147483648,
			8, now()
		)
	`, fixture.WorkerPoolID, WorkerGroup, fixture.VMPlatformID)
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
		INSERT INTO worker_hosts (
			id, resource_id, worker_group_id, worker_pool_id, status,
			current_epoch, current_service_id,
			vm_platform_id,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_vm_starts,
			cpu_environment, cpu_environment_digest,
			observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active', 1, $5,
			$6,
			8000, 8589934592, 17179869184,
			1000, 1073741824, 2147483648,
			8, 8, '{}'::jsonb, $7, now(), now(), now()
		)
	`, fixture.WorkerID, fixture.WorkerID.String(), WorkerGroup,
		fixture.WorkerPoolID, uuid.NewV7(), fixture.VMPlatformID,
		fixture.CPUConfigDigest)
	return fixture
}

func (fixture Fixture) AddRunLease(t *testing.T, state string, createdAt time.Time) RunLease {
	t.Helper()
	ctx := t.Context()
	computerID := uuid.NewV7()
	versionID := uuid.NewV7()
	runID := uuid.NewV7()
	instanceID := uuid.NewV7()
	leaseID := uuid.NewV7()
	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computers (
			id, environment_id, region_id,
			sandbox_declared_id,
			writer_generation, head_disk_version_id
		, computer_spec_id, creation_deployment_id) VALUES (
			$1, $2, $3, 'test-computer',
			2, $5
		, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$4), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$4))
	`, computerID, fixture.EnvironmentID, Region,
		fixture.ComputerDefinitionID, versionID)
	dbtest.InsertCommittedComputerRoot(t, ctx, tx, versionID, fixture.EnvironmentID, computerID)
	dbtest.InsertComputerGeneration(t, ctx, tx, fixture.EnvironmentID, computerID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, computer_id, base_computer_disk_version_id, payload,
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
		computerID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, computer_id, base_computer_disk_version_id
		) VALUES ($1, 1, 'task', $2, $3)
	`, runID, computerID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computer_instances (
			id, preparation_expires_at, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_host_id, vm_platform_id,
        computer_spec_id,
			worker_epoch, vm_vcpu_count, cpu_config_digest,
			reserved_cpu_millis, reserved_memory_bytes,
			reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
			computer_id, program_deployment_id, desired_reason, observed_state,
			observed_version, observed_desired_version, ready_at,
            writer_generation, writer_token_hash, writer_expires_at, mount_state, mounted_at, source_disk_version_id
		) VALUES (
			$1, transaction_timestamp() + interval '5 minutes', $2, $3, $4, $5, $6, $7, $8,
        (SELECT computer_spec_id FROM deployment_definitions WHERE id=$9), 1, 1, $12,
			1000, 1073741824, 2147483648, 1,
			$10, $11, 'test', 'ready', 1, 1, now(),
            2, decode(repeat('02',32),'hex'), now() + interval '10 minutes', 'mounted', now(), $13
		)
	`, instanceID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID,
		fixture.VMPlatformID, fixture.ComputerDefinitionID, computerID,
		fixture.DeploymentID, fixture.CPUConfigDigest, versionID)
	var claimedAt, startedAt any
	if state != "assigned" {
		claimedAt = createdAt.Add(time.Second)
	}
	if state == "running" {
		startedAt = createdAt.Add(2 * time.Second)
	}
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, computer_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_host_id,
			worker_epoch, computer_instance_id, requested_cpu_millis,
			requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
			requested_execution_slots, status, created_at, start_deadline_at,
			claimed_at, expires_at, writer_generation, deployment_id, started_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, 1, $8, $9, 1, $10, 1000, 1073741824, 2147483648, 1,
			$11::text, $12, now() + interval '5 minutes', $13,
			now() + interval '10 minutes', 2, $14, $15
		)
	`, leaseID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, runID, computerID, Region, WorkerGroup, fixture.WorkerID, instanceID, state, createdAt, claimedAt, fixture.DeploymentID, startedAt)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1, first_lease_at = $2
		 WHERE id = $3
	`, leaseID, createdAt, runID)
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
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_computer_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	var computerID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM runs WHERE id = $1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest
) VALUES (
    $1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb,
    decode(repeat('05', 32), 'hex')
) ON CONFLICT ON CONSTRAINT deployment_definitions_membership_key DO NOTHING`, actorDefinitionID, fixture.EnvironmentID, fixture.DeploymentID)
	if err := tx.QueryRow(ctx, `SELECT id FROM deployment_definitions WHERE environment_id=$1 AND deployment_id=$2 AND kind='actor' AND declared_id='test-actor'`, fixture.EnvironmentID, fixture.DeploymentID).Scan(&actorDefinitionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO sessions (
    id, environment_id,
    actor_declared_id, deployment_definition_id, computer_id, current_run_id,
    next_input_sequence, committed_input_sequence,
    run_queue_name, run_max_active_duration_ms, run_retry_policy
) VALUES (
    $1, $2,
    'test-actor', $3, $4, $5,
    3, 1, 'default', 300000, $6::jsonb
)`, actorID, fixture.EnvironmentID, actorDefinitionID, computerID, work.RunID, retryPolicy)
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
