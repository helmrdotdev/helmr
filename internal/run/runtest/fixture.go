package runtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Region         = "us-east-1"
	WorkerGroup    = "run-workers"
	WorkerProtocol = "helmr.worker.v0"
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
	RuntimeIdentityID     string
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
		OrgID:                 uuid.Must(uuid.NewV7()),
		ProjectID:             uuid.Must(uuid.NewV7()),
		EnvironmentID:         uuid.Must(uuid.NewV7()),
		DeploymentID:          uuid.Must(uuid.NewV7()),
		TaskDefinitionID:      uuid.Must(uuid.NewV7()),
		WorkspaceDefinitionID: uuid.Must(uuid.NewV7()),
		WorkerID:              uuid.Must(uuid.NewV7()),
		RuntimeIdentityID:     "run-lease-test-runtime",
	}
	sourceID := uuid.Must(uuid.NewV7())
	programID := uuid.Must(uuid.NewV7())
	imageID := uuid.Must(uuid.NewV7())
	sourceDigest := dbtest.Digest("source")
	programDigest := dbtest.Digest("program")
	imageDigest := dbtest.Digest("image")
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'aws', $1, 'Run Lease Test')
	`, Region)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_groups (
			id, region_id, name, protocol_version, observation_ttl_seconds
		) VALUES ($1, $2, $1, $3, 120)
	`, WorkerGroup, Region, WorkerProtocol)
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
			($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($1, $4, 1, 'application/octet-stream')
	`, fixture.OrgID, sourceDigest, programDigest, imageDigest)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES
			($1, $4, $5, $6, $7, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($2, $4, $5, $6, $8, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($3, $4, $5, $6, $9, 'workspace_image', 1, 'application/octet-stream')
	`, sourceID, programID, imageID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, sourceDigest, programDigest, imageDigest)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployments (
			id, org_id, project_id, environment_id, build_region_id,
			build_node_version, build_runtime_digest, build_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, image_cache_mode, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_index_digest, queue_config, status
		) VALUES (
			$1, $2, $3, $4, $5, '24.16.0',
			decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'prefer', 'run-lease-test', $6, $7, $8,
			decode(repeat('03', 32), 'hex'), '{}'::jsonb, 'deployed'
		)
	`, fixture.DeploymentID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, Region, sourceDigest,
		sourceID, programID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		) VALUES (
			$1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), NULL
		), (
			$2, $3, $4, 'workspace', 'test-workspace', 0, '{}'::jsonb,
			decode(repeat('04', 32), 'hex'), $5
		)
	`, fixture.TaskDefinitionID, fixture.WorkspaceDefinitionID,
		fixture.EnvironmentID, fixture.DeploymentID, imageID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
			rootfs_digest, network_abi
		) VALUES ($1, 'x86_64', 'test', 'kernel', 'initramfs', 'rootfs', 'helmr/v0')
	`, fixture.RuntimeIdentityID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, state,
			current_epoch, current_service_id, protocol_version, supervisor_version,
			supports_run, runtime_identity_id,
			substrate_format, substrate_builder_abi, substrate_layout_abi,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_run_consumers, max_runtime_starts,
			epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, 'active', 1, $4, $5, 'test',
			true, $6, 'squashfs', 'builder-v0', 'layout-v0',
			8000, 8589934592, 17179869184,
			1000, 1073741824, 2147483648,
			8, 8, 8, now(), now()
		)
	`, fixture.WorkerID, fixture.WorkerID.String(), WorkerGroup,
		uuid.Must(uuid.NewV7()), WorkerProtocol, fixture.RuntimeIdentityID)
	return fixture
}

func (fixture Fixture) AddRunLease(t *testing.T, state string, assignedAt time.Time) RunLease {
	t.Helper()
	ctx := t.Context()
	workspaceID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runtimeID := uuid.Must(uuid.NewV7())
	mountID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := uuid.Must(uuid.NewV7())
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
			workspace_declared_id, deployment_definition_id,
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
			worker_epoch, reserved_cpu_millis, reserved_memory_bytes,
			reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
			workspace_id, program_deployment_id, desired_reason, observed_state,
			observed_version, observed_desired_version, preparing_at, ready_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, 1,
			1000, 1073741824, 2147483648, 1,
			$10, $11, 'test', 'ready', 1, 1, now(), now()
		)
	`, runtimeID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID,
		fixture.RuntimeIdentityID, fixture.WorkspaceDefinitionID, workspaceID,
		fixture.DeploymentID)
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
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, 1, $8, $9, 1, $10,
			$11, $12, 1000, 1073741824, 2147483648, 1,
			$13::text, $14, now() + interval '5 minutes', $15,
			now() + interval '10 minutes'
		)
	`, leaseID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, runID,
		workspaceID, Region, WorkerGroup, fixture.WorkerID,
		runtimeID, fixture.RuntimeIdentityID, WorkerProtocol,
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
	actorDefinitionID := uuid.Must(uuid.NewV7())
	actorID := uuid.Must(uuid.NewV7())
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
INSERT INTO actors (
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
   SET owner_actor_id = $1, owner_run_id = NULL
 WHERE id = $2`, actorID, workspaceID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $1,
       entrypoint_kind = 'actor', entrypoint_declared_id = 'test-actor',
       actor_id = $2, cause_kind = 'actor_start',
       actor_start_input_sequence = 1, actor_start_input_high_watermark = 2,
       payload = NULL, retry_policy = $3::jsonb
 WHERE id = $4`, actorDefinitionID, actorID, retryPolicy, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor',
       actor_start_input_sequence = 1
 WHERE run_id = $1 AND number = 1`, work.RunID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return actorID
}

type HandoffChain struct {
	OuterRunID          uuid.UUID
	ParentRunID         uuid.UUID
	OuterWaitID         uuid.UUID
	OuterCheckpoint     uuid.UUID
	OuterResumeID       uuid.UUID
	EnclosingWaitID     uuid.UUID
	EnclosingCheckpoint uuid.UUID
	EnclosingResumeID   uuid.UUID
	RuntimeID           uuid.UUID
	MountID             uuid.UUID
	VersionID           uuid.UUID
}

func (fixture Fixture) AddHandoffChain(
	t *testing.T,
	ctx context.Context,
	work RunLease,
) HandoffChain {
	t.Helper()
	chain := HandoffChain{
		OuterRunID:          uuid.Must(uuid.NewV7()),
		ParentRunID:         uuid.Must(uuid.NewV7()),
		OuterWaitID:         uuid.Must(uuid.NewV7()),
		OuterCheckpoint:     uuid.Must(uuid.NewV7()),
		OuterResumeID:       uuid.Must(uuid.NewV7()),
		EnclosingWaitID:     uuid.Must(uuid.NewV7()),
		EnclosingCheckpoint: uuid.Must(uuid.NewV7()),
		EnclosingResumeID:   uuid.Must(uuid.NewV7()),
	}
	outerClaimID, enclosingClaimID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerLeaseID, parentLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerWorkspaceLeaseID, parentWorkspaceLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	historicalWaitID := uuid.Must(uuid.NewV7())

	tx, err := fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT run_leases.runtime_instance_id,
		       workspace_leases.workspace_mount_id,
		       workspace_leases.base_version_id
		  FROM run_leases
		  JOIN workspace_leases
		    ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE run_leases.id = $1
	`, work.LeaseID).Scan(&chain.RuntimeID, &chain.MountID, &chain.VersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       writer_generation = 3,
		       released_at = now(),
		       terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       claimed_at = assigned_at,
		       started_at = assigned_at,
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at
		) VALUES
			($1, $3, 'task.child.invoke', $4, $5, now()),
			($2, $3, 'task.child.invoke', $6, $7, now())
	`, outerClaimID, enclosingClaimID, fixture.EnvironmentID,
		dbtest.Hash("outer-slot"), dbtest.Hash("outer-request"),
		dbtest.Hash("inner-slot"), dbtest.Hash("inner-request"))
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
			base_workspace_version_id, payload, queue_name, queue_origin_at,
			queue_score_at, max_active_duration_ms, retry_policy, trace_id,
			root_span_id, claim_id
		) VALUES (
			$1, $3, $4, $5, $6, $7, 'task', 'test-task', 'api',
			NULL, NULL, $8, $9, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'33333333333333333333333333333333', '4444444444444444', NULL
		), (
			$2, $3, $4, $5, $6, $7, 'task', 'test-task', 'child',
			$1, true, $8, $9, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'55555555555555555555555555555555', '6666666666666666', $10
		)
	`, chain.OuterRunID, chain.ParentRunID,
		fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID,
		fixture.DeploymentID, fixture.TaskDefinitionID, fixture.workspaceID(t, ctx, tx, work.RunID),
		chain.VersionID, outerClaimID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE runs
		   SET cause_kind = 'child',
		       parent_run_id = $1,
		       parent_owns_lifecycle = true,
		       claim_id = $2
		 WHERE id = $3
	`, chain.ParentRunID, enclosingClaimID, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id,
			entrypoint_entered_at, base_workspace_version_id
		) VALUES
			($1, 1, 'task', $3, now(), $4),
			($2, 1, 'task', $3, now(), $4)
	`, chain.OuterRunID, chain.ParentRunID, fixture.workspaceID(t, ctx, tx, work.RunID), chain.VersionID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE workspaces
		   SET owner_run_id = $1,
		       writer_generation = 3
		 WHERE id = (SELECT workspace_id FROM runs WHERE id = $2)
	`, chain.OuterRunID, work.RunID)

	fixture.parkHandoff(t, ctx, tx, handoffPark{
		runID: chain.OuterRunID, childRunID: chain.ParentRunID,
		claimID: outerClaimID, leaseID: outerLeaseID,
		workspaceLeaseID: outerWorkspaceLeaseID, waitID: chain.OuterWaitID,
		checkpointID: chain.OuterCheckpoint, writerGeneration: 1,
		childWriterGeneration: 2, RuntimeID: chain.RuntimeID,
		MountID: chain.MountID, VersionID: chain.VersionID,
		resumeAttachID: chain.OuterResumeID,
	})
	fixture.parkHandoff(t, ctx, tx, handoffPark{
		runID: chain.ParentRunID, childRunID: work.RunID,
		claimID: enclosingClaimID, leaseID: parentLeaseID,
		workspaceLeaseID: parentWorkspaceLeaseID, waitID: chain.EnclosingWaitID,
		checkpointID: chain.EnclosingCheckpoint, writerGeneration: 2,
		childWriterGeneration: 3, RuntimeID: chain.RuntimeID,
		MountID: chain.MountID, VersionID: chain.VersionID,
		resumeAttachID: chain.EnclosingResumeID,
	})
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, resume_attach_id,
			condition_error, condition_terminal_at, condition_reason_code,
			suspension_terminal_at, suspension_reason_code, suspension_error
		) VALUES (
			$1, $2, $3, $4, 'child', 'failed',
			$5, true, 'test-task', $6, '{}'::jsonb, 'failed',
			1, 1, $7, '{}'::jsonb, now(), 'test_history',
			now(), 'test_history', '{}'::jsonb
		)
	`, historicalWaitID, fixture.EnvironmentID, chain.OuterRunID,
		fixture.workspaceID(t, ctx, tx, work.RunID), chain.ParentRunID,
		outerClaimID, uuid.Must(uuid.NewV7()))
	dbtest.MustExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'assigned',
		       claimed_at = NULL,
		       started_at = NULL,
		       checkpointed_at = NULL,
		       terminal_at = NULL,
		       terminal_reason_code = NULL
		 WHERE id = $1
	`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'active',
		       released_at = NULL,
		       terminal_at = NULL
		 WHERE owner_run_lease_id = $1
	`, work.LeaseID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return chain
}

type handoffPark struct {
	runID                 uuid.UUID
	childRunID            uuid.UUID
	claimID               uuid.UUID
	leaseID               uuid.UUID
	workspaceLeaseID      uuid.UUID
	waitID                uuid.UUID
	checkpointID          uuid.UUID
	writerGeneration      int64
	childWriterGeneration int64
	RuntimeID             uuid.UUID
	MountID               uuid.UUID
	VersionID             uuid.UUID
	resumeAttachID        uuid.UUID
}

func (fixture Fixture) parkHandoff(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	park handoffPark,
) {
	t.Helper()
	workspaceID := fixture.workspaceID(t, ctx, tx, park.runID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id,
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, started_at, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
		       1, 1, worker_group_id, worker_instance_id, worker_epoch,
		       runtime_instance_id,
		       runtime_identity_id, worker_protocol_version, requested_cpu_millis,
		       requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
		       requested_execution_slots, 'running',
		       now() - interval '1 minute', now() + interval '5 minutes',
		       now() - interval '1 minute', now() - interval '1 minute',
		       now() + interval '10 minutes'
		  FROM run_leases
		 WHERE runtime_instance_id = $3
		 ORDER BY created_at
		 LIMIT 1
	`, park.leaseID, park.runID, park.RuntimeID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_token_hash, expires_at
		)
		SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
		       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
		       workspace_mount_id, $2, base_version_id, ownership_generation, $3,
		       mount_fencing_generation, fencing_token_hash,
		       now() + interval '10 minutes'
		  FROM workspace_leases
		 WHERE workspace_id = $4
		 ORDER BY acquired_at
		 LIMIT 1
	`, park.workspaceLeaseID, park.leaseID, park.writerGeneration, workspaceID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1,
		       status = 'running',
		       first_lease_at = now() - interval '1 minute',
		       started_at = now() - interval '1 minute'
		 WHERE id = $2
	`, park.leaseID, park.runID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, current_run_lease_id,
			checkpoint_request_version, checkpoint_ack_version, resume_attach_id
		) VALUES (
			$1, $2, $3, $4, 'child', 'pending',
			$5, true, 'test-task', $6, '{}'::jsonb, 'hot',
			1, 1, $7, 1, 0, $8
		)
	`, park.waitID, fixture.EnvironmentID, park.runID, workspaceID,
		park.childRunID, park.claimID, park.leaseID, park.resumeAttachID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, park.leaseID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       released_at = now(),
		       terminal_at = now()
		 WHERE id = $1
	`, park.workspaceLeaseID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO run_checkpoints (
			id, kind, run_id, attempt_number, run_wait_id,
			source_run_lease_id, source_workspace_lease_id, workspace_id,
			base_workspace_version_id, private_workspace_version_id,
			state, restore_manifest, ready_request_fingerprint, ready_at
		) VALUES (
			$1, 'suspend', $2, 1, $3, $4, $5, $6,
			$7, $7, 'ready', '{"test":true}'::jsonb, 'test-ready', now()
		)
	`, park.checkpointID, park.runID, park.waitID, park.leaseID,
		park.workspaceLeaseID, workspaceID, park.VersionID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE run_waits
		   SET suspension_state = 'parked',
		       current_run_lease_id = NULL,
		       prior_run_lease_id = $1,
		       checkpoint_ack_version = 1,
		       suspend_checkpoint_id = $2,
		       base_workspace_version_id = $3,
		       base_workspace_content_digest = (
		           SELECT content_digest
		             FROM workspace_versions
		            WHERE id = $3
		       ),
		       handoff_runtime_instance_id = $4,
		       handoff_workspace_mount_id = $5,
		       handoff_mount_generation = 2,
		       ownership_generation = 1,
		       parent_writer_generation = $6,
		       child_writer_generation = $7
		 WHERE id = $8
	`, park.leaseID, park.checkpointID, park.VersionID, park.RuntimeID,
		park.MountID, park.writerGeneration, park.childWriterGeneration, park.waitID)
	dbtest.MustExec(t, ctx, tx, `
		UPDATE runs
		   SET status = 'waiting',
		       current_run_lease_id = NULL
		 WHERE id = $1
	`, park.runID)
}

func (fixture Fixture) workspaceID(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	runID uuid.UUID,
) uuid.UUID {
	t.Helper()
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, runID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	return workspaceID
}
