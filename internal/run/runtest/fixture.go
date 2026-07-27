package runtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
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
	sourceDigest := Digest("source")
	programDigest := Digest("program")
	imageDigest := Digest("image")
	programReceipt := dbtest.ProgramReceipt(dbtest.ProgramReceiptAuthority{
		Architecture:            "x86_64",
		ProgramArtifactID:       programID,
		ProgramDigest:           programDigest,
		ProgramSizeBytes:        1,
		RuntimeDigest:           "sha256:" + strings.Repeat("01", 32),
		SourceArtifactID:        sourceID,
		SourceDigest:            sourceDigest,
		SourceSizeBytes:         1,
		StandardToolchainDigest: "sha256:" + strings.Repeat("02", 32),
	})
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'aws', $1, 'Run Lease Test')
	`, Region)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO lookup_hmac_versions (version, key_fingerprint, is_current)
		VALUES (1, $1, true)
	`, Hash("lookup-hmac"))
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_groups (
			id, region_id, name, enrollment_policy_fingerprint,
			allowed_attestation_fingerprints, protocol_version
		) VALUES ($1, $2, $1, 'test-policy', ARRAY['test-attestation'], $3)
	`, WorkerGroup, Region, WorkerProtocol)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Run Lease Test', $3)
	`, fixture.OrgID, PublicID(t, publicid.Organization), "run-lease-"+ShortID(fixture.OrgID))
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Run Lease Test')
	`, fixture.ProjectID, PublicID(t, publicid.Project), fixture.OrgID,
		Region, "run-lease-"+ShortID(fixture.ProjectID))
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, $5, 'Run Lease Test', '#3366ff')
	`, fixture.EnvironmentID, PublicID(t, publicid.Environment), fixture.OrgID,
		fixture.ProjectID, "run-lease-"+ShortID(fixture.EnvironmentID))
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES
			($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($1, $4, 1, 'application/octet-stream')
	`, fixture.OrgID, sourceDigest, programDigest, imageDigest)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES
			($1, $4, $5, $6, $7, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($2, $4, $5, $6, $8, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($3, $4, $5, $6, $9, 'workspace_image', 1, 'application/octet-stream')
	`, sourceID, programID, imageID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, sourceDigest, programDigest, imageDigest)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployments (
			id, public_id, org_id, project_id, environment_id, build_region_id,
			build_architecture, build_runtime_digest, build_standard_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_runtime_digest, program_architecture,
			program_receipt, queue_config, status
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'x86_64',
			decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'bun', '1.2.3', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'run-lease-test', $7, $8, $9,
			decode(repeat('01', 32), 'hex'), 'x86_64', $10::jsonb, '{}'::jsonb, 'deployed'
		)
	`, fixture.DeploymentID, PublicID(t, publicid.Deployment), fixture.OrgID,
		fixture.ProjectID, fixture.EnvironmentID, Region, sourceDigest,
		sourceID, programID, programReceipt)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest,
			workspace_architecture, artifact_id
		) VALUES (
			$1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), NULL, NULL
		), (
			$2, $3, $4, 'workspace', 'test-workspace', 0, '{}'::jsonb,
			decode(repeat('04', 32), 'hex'), 'x86_64', $5
		)
	`, fixture.TaskDefinitionID, fixture.WorkspaceDefinitionID,
		fixture.EnvironmentID, fixture.DeploymentID, imageID)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
			rootfs_digest, cni_profile
		) VALUES ($1, 'x86_64', 'test', 'kernel', 'initramfs', 'rootfs', 'default')
	`, fixture.RuntimeIdentityID)
	MustExec(t, t.Context(), fixture.Pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version, supervisor_version,
			supports_run, runtime_identity_id,
			substrate_format, substrate_builder_abi, substrate_layout_abi,
			certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
			certified_scratch_bytes, per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_workload_disk_bytes, per_vm_scratch_bytes,
			max_vm_slots, max_run_consumers, max_runtime_starts,
			certification_profile, certification_fingerprint,
			epoch_started_at, certified_at, activated_at
		) VALUES (
			$1, $2, $3, 'test-attestation', 'active', 1, $4, $5, 'test',
			true, $6, 'squashfs', 'builder-v0', 'layout-v0',
			8000, 8589934592, 17179869184, 17179869184,
			1000, 1073741824, 2147483648, 2147483648,
			8, 8, 8, 'test', 'test-cert', now(), now(), now()
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
	slotID := uuid.Must(uuid.NewV7())
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
	MustExec(t, ctx, tx, `
		INSERT INTO workspaces (
			id, public_id, environment_id, region_id,
			workspace_declared_id, deployment_definition_id,
			owner_run_id, ownership_generation, writer_generation, head_version_id
		) VALUES (
			$1, $2, $3, $4, 'test-workspace', $5,
			$6, 1, 1, $7
		)
	`, workspaceID, PublicID(t, publicid.Workspace),
		fixture.EnvironmentID, Region,
		fixture.WorkspaceDefinitionID, runID, versionID)
	MustExec(t, ctx, tx, `
		INSERT INTO workspace_versions (
			id, public_id, environment_id, workspace_id,
			kind, content_digest, state, ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, $4, 'system',
			'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			'committed', 0, 0, now()
		)
	`, versionID, PublicID(t, publicid.WorkspaceVersion),
		fixture.EnvironmentID, workspaceID)
	MustExec(t, ctx, tx, `
		INSERT INTO runs (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, workspace_id, base_workspace_version_id, payload,
			queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
			retry_policy, trace_id, root_span_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 'task', 'test-task', 'api',
			$8, $9, '{}'::jsonb, 'default', now(), now(), 300000,
			'{"enabled":false}'::jsonb,
			'11111111111111111111111111111111', '2222222222222222'
		)
	`, runID, PublicID(t, publicid.Run), fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, fixture.DeploymentID, fixture.TaskDefinitionID,
		workspaceID, versionID)
	MustExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
		) VALUES ($1, 1, 'task', $2, $3)
	`, runID, workspaceID, versionID)
	MustExec(t, ctx, tx, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_definition_id,
			worker_epoch, network_policy, reserved_cpu_millis, reserved_memory_bytes,
			reserved_workload_disk_bytes, reserved_scratch_bytes, reserved_execution_slots,
			workspace_id, program_deployment_id, desired_reason, observed_state,
			observed_version, observed_desired_version, preparing_at, ready_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, 1, '{}'::jsonb,
			1000, 1073741824, 2147483648, 2147483648, 1,
			$10, $11, 'test', 'ready', 1, 1, now(), now()
		)
	`, runtimeID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID,
		fixture.RuntimeIdentityID, fixture.WorkspaceDefinitionID, workspaceID,
		fixture.DeploymentID)
	MustExec(t, ctx, tx, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, runtime_instance_id, host_interface_name,
			guest_address, gateway_address, subnet, tap_name, netns_name,
			guest_mac, assigned_at
		) VALUES (
			$1, $2, $3, 1, $4, 1, 'bound', $5, $6,
			$9, '10.0.0.1', '10.0.0.0/8', $7, $8,
			'02:00:00:00:00:01', now()
		)
	`, slotID, WorkerGroup, fixture.WorkerID, "slot-"+ShortID(slotID),
		runtimeID, "veth-"+ShortID(slotID), "tap-"+ShortID(slotID),
		"netns-"+ShortID(slotID),
		fmt.Sprintf("10.%d.%d.%d", slotID[13], slotID[14], slotID[15]))
	MustExec(t, ctx, tx, `
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
	MustExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, 1, $8, $9, 1, $10, $11, 1,
			$12, $13, 1000, 1073741824, 2147483648, 2147483648, 1,
			$14::text, $15, now() + interval '5 minutes', $16,
			now() + interval '10 minutes'
		)
	`, leaseID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, runID,
		workspaceID, Region, WorkerGroup, fixture.WorkerID,
		runtimeID, slotID, fixture.RuntimeIdentityID, WorkerProtocol,
		state, assignedAt, claimedAt)
	MustExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_key_fingerprint, fencing_token_hash, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, $11, $12,
			1, 1, 2, decode(repeat('00', 32), 'hex'), 'test-token-hash',
			now() + interval '10 minutes'
		)
	`, workspaceLeaseID, fixture.OrgID, WorkerGroup, fixture.ProjectID,
		fixture.EnvironmentID, Region, fixture.WorkerID, runtimeID,
		workspaceID, mountID, leaseID, versionID)
	MustExec(t, ctx, tx, `
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
	MustExec(t, ctx, fixture.Pool, `
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
	MustExec(t, ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest
) VALUES (
    $1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb,
    decode(repeat('05', 32), 'hex')
)`, actorDefinitionID, fixture.EnvironmentID, fixture.DeploymentID)
	MustExec(t, ctx, tx, `
INSERT INTO actors (
    id, public_id, environment_id,
    actor_declared_id, deployment_definition_id, workspace_id, current_run_id,
    next_input_sequence, committed_input_sequence,
    run_queue_name, run_max_active_duration_ms, run_retry_policy
) VALUES (
    $1, $2, $3,
    'test-actor', $4, $5, $6,
    3, 1, 'default', 300000, $7::jsonb
)`, actorID, "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		fixture.EnvironmentID, actorDefinitionID, workspaceID, work.RunID, retryPolicy)
	MustExec(t, ctx, tx, `
UPDATE workspaces
   SET owner_actor_id = $1, owner_run_id = NULL
 WHERE id = $2`, actorID, workspaceID)
	MustExec(t, ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $1,
       entrypoint_kind = 'actor', entrypoint_declared_id = 'test-actor',
       actor_id = $2, cause_kind = 'actor_start',
       actor_start_input_sequence = 1, actor_start_input_high_watermark = 2,
       payload = NULL, retry_policy = $3::jsonb
 WHERE id = $4`, actorDefinitionID, actorID, retryPolicy, work.RunID)
	MustExec(t, ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor',
       actor_start_input_sequence = 1
 WHERE run_id = $1 AND number = 1`, work.RunID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return actorID
}

func MustExec(t *testing.T, ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func PublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	value, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func Digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Hash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func ShortID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")[20:]
}
