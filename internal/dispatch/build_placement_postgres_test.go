package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgxpool"
)

type buildPlacementFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	authority     *Authority
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	groupID       string
}

func newBuildPlacementFixture(t *testing.T) *buildPlacementFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	authority, err := NewBuildAuthority(pool)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &buildPlacementFixture{
		ctx: ctx, pool: pool, authority: authority,
		orgID: uuid.Must(uuid.NewV7()), projectID: uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()), deploymentID: uuid.Must(uuid.NewV7()),
		groupID: "build-placement-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	sourceArtifactID := uuid.Must(uuid.NewV7())
	sourceDigest := "sha256:" + strings.Repeat("1", 64)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO regions (id, provider, provider_region, display_name)
VALUES ('us-east-1', 'aws', 'us-east-1', 'US East')`)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2)`,
		fixture.orgID, "org-"+fixture.orgID.String())
	mustDispatchExec(t, ctx, pool, `
INSERT INTO projects (id, org_id, default_region_id, slug, name)
VALUES ($1, $2, 'us-east-1', $3, 'Project')`,
		fixture.projectID, fixture.orgID,
		"project-"+fixture.projectID.String())
	mustDispatchExec(t, ctx, pool, `
INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, 'Environment', '#3366ff')`,
		fixture.environmentID, fixture.orgID,
		fixture.projectID, "environment-"+fixture.environmentID.String())
	mustDispatchExec(t, ctx, pool, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar')`,
		fixture.orgID, sourceDigest)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES ($1, $2, $3, $4, $5, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar')`,
		sourceArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID, sourceDigest)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, build_region_id,
    build_node_version, build_runtime_digest, build_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract_version, image_cache_mode, version, content_hash,
    deployment_source_artifact_id, status
) VALUES (
    $1, $2, $3, $4, 'us-east-1', '24.16.0', $5,
    decode(repeat('02', 32), 'hex'),
    'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
    'helmr.program-build.v0', 'prefer',
    'v1', $6, $7, 'queued'
)`,
		fixture.deploymentID, fixture.orgID, fixture.projectID,
		fixture.environmentID, bytes.Repeat([]byte{1}, 32),
		"sha256:"+strings.Repeat("2", 64), sourceArtifactID)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO worker_groups (
    id, region_id, name, observation_ttl_seconds
) VALUES ($1, 'us-east-1', $1, 120)`,
		fixture.groupID)
	return fixture
}

func (f *buildPlacementFixture) addWorker(t *testing.T, ready bool) uuid.UUID {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, network_abi
) VALUES ($1, $2, 'helmr.runtime.v0', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'helmr/v0')`,
		runtimeID, platformArchitecture)
	if ready {
		mustDispatchExec(t, f.ctx, f.pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, state,
			current_epoch, current_service_id, protocol_version, supervisor_version, supports_build,
			runtime_identity_id,
			epoch_cpu_millis, epoch_memory_bytes,
    epoch_guest_ephemeral_disk_bytes, per_vm_cpu_millis,
    per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_build_executors, epoch_started_at, activated_at
) VALUES (
			$1, $2, $3, 'active',
			1, $4, 'helmr.worker.v0', 'test-worker', true, $5, 3000, 4294967296, 34359738368,
			2000, 2147483648, 34359738368, 1, now(), now()
)`, workerID, workerID.String(), f.groupID, serviceID, runtimeID)
	} else {
		mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, state,
    current_epoch, current_service_id, protocol_version, supports_build,
    runtime_identity_id, epoch_started_at
) VALUES (
    $1, $2, $3, 'registering',
    1, $4, 'helmr.worker.v0', true, $5, now()
)`, workerID, workerID.String(), f.groupID, serviceID, runtimeID)
	}
	mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    guest_ephemeral_disk_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, quarantined_resource_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth, observed_at
) VALUES ($1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, now())`, workerID)
	return workerID
}

func (f *buildPlacementFixture) candidate() ReadyBuildCandidate {
	return ReadyBuildCandidate{
		OrgID: pgvalue.UUID(f.orgID), DeploymentID: pgvalue.UUID(f.deploymentID),
		BuildRegionID: "us-east-1", LeaseSequence: 1,
	}
}

func TestPlaceReadyBuildExcludesIneligibleWorkers(t *testing.T) {
	for _, test := range []struct {
		name                 string
		ready                bool
		insufficientEpochCPU bool
	}{
		{name: "insufficient epoch CPU", ready: true, insufficientEpochCPU: true},
		{name: "unready worker", ready: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBuildPlacementFixture(t)
			workerID := fixture.addWorker(t, test.ready)
			if test.insufficientEpochCPU {
				mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET epoch_cpu_millis = 2999
 WHERE id = $1`, workerID)
			}
			_, err := fixture.authority.PlaceReadyBuild(
				fixture.ctx, fixture.candidate(),
			)
			if !errors.Is(err, ErrCapacityUnavailable) {
				t.Fatalf("PlaceReadyBuild() error = %v, want ErrCapacityUnavailable", err)
			}
			var leases int
			if err := fixture.pool.QueryRow(fixture.ctx,
				`SELECT count(*) FROM deployment_build_leases WHERE deployment_id = $1`,
				fixture.deploymentID).Scan(&leases); err != nil {
				t.Fatal(err)
			}
			if leases != 0 {
				t.Fatalf("created %d build leases for an ineligible worker", leases)
			}
		})
	}
}

func TestPlaceReadyBuildRequiresV0WorkerRuntimeIdentity(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	workerID := fixture.addWorker(t, true)
	lease, err := fixture.authority.PlaceReadyBuild(
		fixture.ctx, fixture.candidate(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.WorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("placed worker = %v, want %s", lease.WorkerInstanceID, workerID)
	}
	var architecture string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_identities.runtime_arch
  FROM deployment_build_leases
  JOIN worker_instances ON worker_instances.id = deployment_build_leases.worker_instance_id
  JOIN runtime_identities ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE deployment_build_leases.id = $1`, lease.ID).Scan(&architecture); err != nil {
		t.Fatal(err)
	}
	if architecture != platformArchitecture {
		t.Fatalf("leased worker architecture = %q, want %s", architecture, platformArchitecture)
	}
}

func newDispatchIntegrationDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	return database.Pool
}

func mustDispatchExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
