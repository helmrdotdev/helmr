package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type runPlacementFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	authority     *Authority
	fencingKeys   workspace.FencingKeys
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	runID         uuid.UUID
	workspaceID   uuid.UUID
	workerID      uuid.UUID
	groupID       string
}

func TestPlaceReadyRunPreparesMountAndGrantsFencedLeases(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := fixture.candidate()
	freshAfter := pgvalue.Timestamptz(time.Now().Add(-time.Minute))

	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.LeaseCreated ||
		!reserved.RuntimeInstanceID.Valid ||
		reserved.WorkspaceMountID.Valid {
		t.Fatalf("reservation placement = %+v", reserved)
	}

	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'ready',
       observed_version = 1,
       observed_desired_version = desired_version,
       preparing_at = transaction_timestamp(),
       ready_at = transaction_timestamp(),
       observed_at = transaction_timestamp()
 WHERE id = $1`,
		reserved.RuntimeInstanceID,
	)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_network_slots
   SET state = 'bound',
       host_interface_name = 'veth-test',
       guest_address = '10.0.0.2',
       gateway_address = '10.0.0.1',
       subnet = '10.0.0.0/24',
       tap_name = 'tap-test',
       netns_name = 'netns-test',
       guest_mac = '02:00:00:00:00:01'
 WHERE runtime_instance_id = $1`,
		reserved.RuntimeInstanceID,
	)

	mounting, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if mounting.LeaseCreated ||
		!mounting.WorkspaceMountID.Valid ||
		mounting.RuntimeInstanceID != reserved.RuntimeInstanceID {
		t.Fatalf("mounting placement = %+v", mounting)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'mounted',
       mounted_at = transaction_timestamp()
 WHERE id = $1`,
		mounting.WorkspaceMountID,
	)

	granted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated ||
		!granted.Lease.ID.Valid ||
		granted.Lease.RuntimeInstanceID != reserved.RuntimeInstanceID {
		t.Fatalf("granted placement = %+v", granted)
	}

	var currentLeaseID, reservedRunID, workspaceLeaseID pgtype.UUID
	var firstLeaseAt pgtype.Timestamptz
	var stateVersion, writerGeneration, mountGeneration int64
	var ownerRunLeaseID pgtype.UUID
	var keyFingerprint []byte
	var tokenHash string
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.current_run_lease_id,
       runs.first_lease_at,
       runs.state_version,
       runtime_instances.reserved_run_id,
       workspaces.writer_generation,
       workspace_mounts.fencing_generation,
       workspace_leases.id,
       workspace_leases.owner_run_lease_id,
       workspace_leases.fencing_key_fingerprint,
       workspace_leases.fencing_token_hash
  FROM runs
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.id = $1`,
		fixture.runID,
	).Scan(
		&currentLeaseID,
		&firstLeaseAt,
		&stateVersion,
		&reservedRunID,
		&writerGeneration,
		&mountGeneration,
		&workspaceLeaseID,
		&ownerRunLeaseID,
		&keyFingerprint,
		&tokenHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if currentLeaseID != granted.Lease.ID ||
		ownerRunLeaseID != granted.Lease.ID ||
		!firstLeaseAt.Valid ||
		stateVersion != 2 ||
		reservedRunID.Valid ||
		writerGeneration != 1 ||
		mountGeneration != 2 {
		t.Fatalf(
			"grant receipt lease=%s owner=%s first=%v state=%d reserved=%s writer=%d mount=%d",
			pgvalue.UUIDString(currentLeaseID),
			pgvalue.UUIDString(ownerRunLeaseID),
			firstLeaseAt.Valid,
			stateVersion,
			pgvalue.UUIDString(reservedRunID),
			writerGeneration,
			mountGeneration,
		)
	}
	leaseUUID, err := pgvalue.UUIDValue(workspaceLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.fencingKeys.DeriveActive(workspace.FenceInput{
		LeaseID:                leaseUUID,
		WorkspaceID:            fixture.workspaceID,
		OwnershipGeneration:    1,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyFingerprint, replayed.KeyFingerprint.Bytes()) ||
		tokenHash != replayed.Hash ||
		tokenHash == replayed.Token {
		t.Fatal("Workspace Lease did not persist the replayable fingerprint and token hash")
	}
}

func TestPlaceReadyRunRejectsPerVMIncompatibleWorkspace(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET per_vm_memory_bytes = 536870912
 WHERE id = $1`,
		fixture.workerID,
	)

	_, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
		pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
	)
	if err != ErrCapacityUnavailable {
		t.Fatalf("PlaceReadyRun() error = %v, want ErrCapacityUnavailable", err)
	}
	var runtimes int
	if err := fixture.pool.QueryRow(
		fixture.ctx,
		`SELECT count(*) FROM runtime_instances WHERE workspace_id = $1`,
		fixture.workspaceID,
	).Scan(&runtimes); err != nil {
		t.Fatal(err)
	}
	if runtimes != 0 {
		t.Fatalf("created %d runtimes for an incompatible per-VM profile", runtimes)
	}
}

func TestPlaceReadyRunAccountsForActiveBuildResources(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET certified_memory_bytes = 4294967296,
       certified_scratch_bytes = 68719476736,
       per_vm_scratch_bytes = 68719476736
 WHERE id = $1`,
		fixture.workerID,
	)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
INSERT INTO deployment_build_leases (
    id, org_id, project_id, environment_id, deployment_id, build_region_id,
    lease_sequence, worker_group_id, worker_instance_id, worker_epoch,
    worker_protocol_version, requested_cpu_millis, requested_memory_bytes,
    requested_workload_disk_bytes, requested_scratch_bytes,
    requested_build_executors, build_snapshot, start_deadline_at, expires_at
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 1, $6, $7, 1,
    'helmr.worker.v0', 3000, 4294967296, 0, 34359738368,
    1, '{}'::jsonb, now() + interval '1 minute', now() + interval '5 minutes'
)`,
		uuid.Must(uuid.NewV7()),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		fixture.deploymentID,
		fixture.groupID,
		fixture.workerID,
	)

	_, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
		pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
	)
	if err != ErrCapacityUnavailable {
		t.Fatalf("PlaceReadyRun() error = %v, want ErrCapacityUnavailable", err)
	}
}

func (fixture runPlacementFixture) candidate() ReadyRunCandidate {
	return ReadyRunCandidate{
		OrgID:                   pgvalue.UUID(fixture.orgID),
		RunID:                   pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 1,
	}
}

func newRunPlacementFixture(t *testing.T) runPlacementFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	fixture := runPlacementFixture{
		ctx:           ctx,
		pool:          pool,
		orgID:         uuid.Must(uuid.NewV7()),
		projectID:     uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()),
		runID:         uuid.Must(uuid.NewV7()),
		workspaceID:   uuid.Must(uuid.NewV7()),
		workerID:      uuid.Must(uuid.NewV7()),
		groupID:       "run-placement-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	key := bytes.Repeat([]byte{7}, workspace.FencingKeySize)
	var fixedKey [workspace.FencingKeySize]byte
	copy(fixedKey[:], key)
	fingerprint := workspace.FencingKeyFingerprintForKey(fixedKey).String()
	var err error
	fixture.fencingKeys, err = workspace.NewFencingKeys(
		fingerprint,
		map[string][]byte{fingerprint: key},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority, err = NewRunAuthority(
		pool,
		fixture.fencingKeys,
		RunPlacementPolicy{
			PreparationLimit: 8,
			ReservationTTL:   5 * time.Minute,
			StartDeadline:    time.Minute,
			LeaseTTL:         5 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID := uuid.Must(uuid.NewV7())
	fixture.deploymentID = deploymentID
	taskDefinitionID := uuid.Must(uuid.NewV7())
	workspaceDefinitionID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	sourceID := uuid.Must(uuid.NewV7())
	codeID := uuid.Must(uuid.NewV7())
	dependenciesID := uuid.Must(uuid.NewV7())
	imageID := uuid.Must(uuid.NewV7())
	runtimeIdentityID := "run-runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	sourceDigest := "sha256:" + strings.Repeat("1", 64)
	codeDigest := "sha256:" + strings.Repeat("2", 64)
	dependenciesDigest := "sha256:" + strings.Repeat("3", 64)
	imageDigest := "sha256:" + strings.Repeat("4", 64)

	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO regions (id, provider, provider_region, display_name)
VALUES ('us-east-1', 'aws', 'us-east-1', 'US East')`)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO organizations (id, public_id, name, slug)
VALUES ($1, $2, 'Org', $3)`,
		fixture.orgID,
		dispatchPublicID(t, publicid.Organization),
		"org-"+fixture.orgID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
VALUES ($1, $2, $3, 'us-east-1', $4, 'Project')`,
		fixture.projectID,
		dispatchPublicID(t, publicid.Project),
		fixture.orgID,
		"project-"+fixture.projectID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, $5, 'Environment', '#3366ff')`,
		fixture.environmentID,
		dispatchPublicID(t, publicid.Environment),
		fixture.orgID,
		fixture.projectID,
		"environment-"+fixture.environmentID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES
    ($1, $2, 1, 'application/octet-stream'),
    ($1, $3, 1, 'application/octet-stream'),
    ($1, $4, 1, 'application/octet-stream'),
    ($1, $5, 1, 'application/octet-stream')`,
		fixture.orgID,
		sourceDigest,
		codeDigest,
		dependenciesDigest,
		imageDigest,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES
    ($1, $5, $6, $7, $8, 'deployment_source', 1, 'application/octet-stream'),
    ($2, $5, $6, $7, $9, 'deployment_program_code', 1, 'application/octet-stream'),
    ($3, $5, $6, $7, $10, 'deployment_program_dependencies', 1, 'application/octet-stream'),
    ($4, $5, $6, $7, $11, 'workspace_image', 1, 'application/octet-stream')`,
		sourceID,
		codeID,
		dependenciesID,
		imageID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		sourceDigest,
		codeDigest,
		dependenciesDigest,
		imageDigest,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO deployments (
    id, public_id, org_id, project_id, environment_id, build_region_id,
    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
    build_contract_version, version, content_hash, deployment_source_artifact_id,
    program_code_artifact_id, program_dependency_artifact_id,
    program_runtime_digest, program_architecture, queue_config, status
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 'aarch64',
    decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
    'helmr.program-build.v0', 'v1', $6, $7, $8, $9,
    decode(repeat('01', 32), 'hex'), 'aarch64', '{}'::jsonb, 'deployed'
)`,
		deploymentID,
		dispatchPublicID(t, publicid.Deployment),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		sourceDigest,
		sourceID,
		codeID,
		dependenciesID,
	)
	workspaceManifest := fmt.Sprintf(
		`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024,"diskMiB":2048},"network":{"internet":true,"denyCidrs":[]},"architecture":"aarch64"}`,
		imageDigest,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id, manifest_version,
    manifest, manifest_digest, workspace_architecture, artifact_id
) VALUES
    ($1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
     decode(repeat('03', 32), 'hex'), NULL, NULL),
    ($2, $3, $4, 'workspace', 'test-workspace', 0, $5::jsonb,
     decode(repeat('04', 32), 'hex'), 'aarch64', $6)`,
		taskDefinitionID,
		workspaceDefinitionID,
		fixture.environmentID,
		deploymentID,
		workspaceManifest,
		imageID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_groups (
    id, region_id, name, enrollment_policy_fingerprint,
    allowed_attestation_fingerprints, allows_run, allows_build
) VALUES ($1, 'us-east-1', $1, 'test-policy', ARRAY['test-attestation'], true, false)`,
		fixture.groupID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
    rootfs_digest, cni_profile
) VALUES ($1, 'aarch64', 'helmr.runtime.v0', 'kernel', 'initramfs', 'rootfs', 'default')`,
		runtimeIdentityID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, attestation_fingerprint, state,
    current_epoch, current_service_id, protocol_version, supervisor_version,
    supports_run, runtime_identity_id,
    substrate_format, substrate_builder_abi, substrate_layout_abi,
    certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
    certified_scratch_bytes, per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_workload_disk_bytes, per_vm_scratch_bytes, max_vm_slots,
    max_run_consumers, max_runtime_starts, certification_profile,
    certification_fingerprint, epoch_started_at, certified_at, activated_at
) VALUES (
    $1, $2, $3, 'test-attestation', 'active', 1, $4, 'helmr.worker.v0',
    'test-worker', true, $5, 'squashfs', 'builder-v0', 'layout-v0',
    8000, 8589934592, 17179869184, 17179869184,
    1000, 1073741824, 2147483648, 2147483648,
    8, 8, 8, 'run-v0', 'test-cert', now(), now(), now()
)`,
		fixture.workerID,
		fixture.workerID.String(),
		fixture.groupID,
		uuid.Must(uuid.NewV7()),
		runtimeIdentityID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    workload_disk_pressure_bps, scratch_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, leaked_slot_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth, observed_at
) VALUES ($1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, now())`,
		fixture.workerID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_network_slots (
    id, worker_group_id, worker_instance_id, worker_epoch, slot_name, generation
) VALUES ($1, $2, $3, 1, 'slot-1', 1)`,
		uuid.Must(uuid.NewV7()),
		fixture.groupID,
		fixture.workerID,
	)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO workspaces (
    id, public_id, org_id, project_id, environment_id, region_id,
    declaration_kind, workspace_declared_id, deployment_definition_id,
    owner_run_id, ownership_generation, writer_generation, head_version_id
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 'workspace', 'test-workspace',
    $6, $7, 1, 0, $8
)`,
		fixture.workspaceID,
		dispatchPublicID(t, publicid.Workspace),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		workspaceDefinitionID,
		fixture.runID,
		versionID,
	)
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO workspace_versions (
    id, public_id, org_id, project_id, environment_id, workspace_id,
    kind, content_digest, state, ownership_generation, writer_generation, published_at
) VALUES (
    $1, $2, $3, $4, $5, $6, 'system',
    'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
    'committed', 0, 0, now()
)`,
		versionID,
		dispatchPublicID(t, publicid.WorkspaceVersion),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		fixture.workspaceID,
	)
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO runs (
    id, public_id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload, queue_name,
    queue_origin_at, queue_score_at, max_active_duration_ms, retry_policy,
    trace_id, root_span_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 'task', 'test-task', 'api', $8, $9,
    '{}'::jsonb, 'default', now(), now(), 300000, '{"enabled":false}'::jsonb,
    '11111111111111111111111111111111', '2222222222222222'
)`,
		fixture.runID,
		dispatchPublicID(t, publicid.Run),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		deploymentID,
		taskDefinitionID,
		fixture.workspaceID,
		versionID,
	)
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`,
		fixture.runID,
		fixture.workspaceID,
		versionID,
	)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func mustRunPlacementExec(
	t *testing.T,
	ctx context.Context,
	execer interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	},
	sql string,
	args ...any,
) {
	t.Helper()
	if _, err := execer.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
