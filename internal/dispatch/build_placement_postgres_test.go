package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/capacityapi"
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
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO regions (id, display_name)
VALUES ('us-east-1', 'US East')`)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2)`,
		fixture.orgID, "org-"+fixture.orgID.String())
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO projects (id, org_id, default_region_id, slug, name)
VALUES ($1, $2, 'us-east-1', $3, 'Project')`,
		fixture.projectID, fixture.orgID,
		"project-"+fixture.projectID.String())
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, 'Environment', '#3366ff')`,
		fixture.environmentID, fixture.orgID,
		fixture.projectID, "environment-"+fixture.environmentID.String())
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar')`,
		fixture.orgID, sourceDigest)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES ($1, $2, $3, $4, $5, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar')`,
		sourceArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID, sourceDigest)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, build_region_id,
    build_node_version, build_runtime_digest, build_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract, image_cache_mode, version, content_hash,
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
	dbtest.MustExec(t, ctx, pool, `
WITH token AS (
    INSERT INTO worker_group_tokens (id, token_hash)
    VALUES ($2, $3)
    RETURNING id
)
INSERT INTO worker_groups (
    id, token_id, region_id, name, allows_run, allows_build
)
SELECT $1, token.id, 'us-east-1', $1, false, true FROM token`,
		fixture.groupID, uuid.Must(uuid.NewV7()), dbtest.Hash("build-placement-worker-group"))
	seedDispatchWorkerPool(t, ctx, pool, fixture.groupID, dispatchWorkerPoolFixture{
		allowsBuild:                     true,
		capacityCPUMillis:               3000,
		capacityMemoryBytes:             4294967296,
		capacityGuestEphemeralDiskBytes: 34359738368,
		perVMCPUMillis:                  2000,
		perVMMemoryBytes:                2147483648,
		perVMGuestEphemeralDiskBytes:    34359738368,
		maxBuildExecutors:               1,
	})
	return fixture
}

func (f *buildPlacementFixture) addWorker(t *testing.T, ready bool) uuid.UUID {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	if ready {
		cpuEnvironment, cpuEnvironmentDigest := dispatchCPUEnvironment(t)
		dbtest.MustExec(t, f.ctx, f.pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, worker_pool_id, state,
			current_epoch, current_service_id, supports_build,
			runtime_identity_id,
			epoch_cpu_millis, epoch_memory_bytes,
    epoch_guest_ephemeral_disk_bytes, per_vm_cpu_millis,
    per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_build_executors, cpu_environment, cpu_environment_digest,
    observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active',
			1, $5, true, $6, 3000, 4294967296, 34359738368,
			2000, 2147483648, 34359738368, 1, $7::jsonb, $8,
			now(), now(), now()
)`, workerID, workerID.String(), f.groupID, dbtest.DefaultWorkerPoolID,
			serviceID, dbtest.DefaultRuntimeID, cpuEnvironment, cpuEnvironmentDigest)
	} else {
		dbtest.MustExec(t, f.ctx, f.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state
) VALUES (
    $1, $2, $3, $4, 'registering'
)`, workerID, workerID.String(), f.groupID, dbtest.DefaultWorkerPoolID)
	}
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
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
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

func TestPlaceReadyBuildRequiresSupportedWorkerRuntimeIdentity(t *testing.T) {
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
	var architecture, contract string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_identities.runtime_arch, runtime_identities.vm_runtime_contract
  FROM deployment_build_leases
  JOIN worker_instances ON worker_instances.id = deployment_build_leases.worker_instance_id
  JOIN runtime_identities ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE deployment_build_leases.id = $1`, lease.ID).Scan(&architecture, &contract); err != nil {
		t.Fatal(err)
	}
	if architecture != platformArchitecture {
		t.Fatalf("leased worker architecture = %q, want %s", architecture, platformArchitecture)
	}
	if contract != capacityapi.RuntimeContract {
		t.Fatalf("leased worker runtime contract = %q, want %s", contract, capacityapi.RuntimeContract)
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
