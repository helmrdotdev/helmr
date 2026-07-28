package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
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

var buildPlacementToolchainCatalogDigest = bytes.Repeat([]byte{3}, 32)

type buildPlacementCatalog struct{}

func (buildPlacementCatalog) Digest() (string, error) {
	return "sha256:" + strings.Repeat("03", 32), nil
}

func (buildPlacementCatalog) Resolve(digest string) (deployment.Toolchain, error) {
	if digest == "sha256:"+strings.Repeat("02", 32) ||
		digest == "sha256:"+strings.Repeat("04", 32) {
		return deployment.Toolchain{}, nil
	}
	return deployment.Toolchain{}, errors.New("standard toolchain is not registered")
}

func newBuildPlacementFixture(t *testing.T) *buildPlacementFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	catalog := buildPlacementCatalog{}
	authority, err := NewBuildAuthority(
		pool,
		buildPlacementToolchainCatalogDigest,
		func(digest string) error {
			_, err := catalog.Resolve(digest)
			return err
		},
	)
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
INSERT INTO organizations (id, public_id, name, slug) VALUES ($1, $2, 'Org', $3)`,
		fixture.orgID, dispatchPublicID(t, publicid.Organization), "org-"+fixture.orgID.String())
	mustDispatchExec(t, ctx, pool, `
INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
VALUES ($1, $2, $3, 'us-east-1', $4, 'Project')`,
		fixture.projectID, dispatchPublicID(t, publicid.Project), fixture.orgID,
		"project-"+fixture.projectID.String())
	mustDispatchExec(t, ctx, pool, `
INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, $5, 'Environment', '#3366ff')`,
		fixture.environmentID, dispatchPublicID(t, publicid.Environment), fixture.orgID,
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
    id, public_id, org_id, project_id, environment_id, build_region_id,
    build_node_version, build_runtime_digest, build_standard_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract_version, version, content_hash,
    deployment_source_artifact_id, status
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', '24.16.0', $6,
    decode(repeat('02', 32), 'hex'),
    'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
    'helmr.program-build.v0',
    'v1', $7, $8, 'queued'
)`,
		fixture.deploymentID, dispatchPublicID(t, publicid.Deployment), fixture.orgID,
		fixture.projectID, fixture.environmentID, bytes.Repeat([]byte{1}, 32),
		"sha256:"+strings.Repeat("2", 64), sourceArtifactID)
	mustDispatchExec(t, ctx, pool, `
INSERT INTO worker_groups (
    id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints
) VALUES ($1, 'us-east-1', $1, 'sha256:test-policy', ARRAY['sha256:test-attestation'])`,
		fixture.groupID)
	return fixture
}

func (f *buildPlacementFixture) addWorker(t *testing.T, certified bool) uuid.UUID {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
) VALUES ($1, $2, 'helmr.runtime.v0', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'helmr/v0')`,
		runtimeID, platformArchitecture)
	if certified {
		mustDispatchExec(t, f.ctx, f.pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version, supervisor_version, supports_build,
			toolchain_catalog_digest,
			runtime_identity_id, certified_cpu_millis, certified_memory_bytes,
    certified_workload_disk_bytes, certified_scratch_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_workload_disk_bytes,
    per_vm_scratch_bytes, max_build_executors, certification_profile,
    certification_fingerprint, epoch_started_at, certified_at, activated_at
) VALUES (
			$1, $2, $3, 'sha256:test-attestation', 'active',
			1, $4, 'helmr.worker.v0', 'test-worker', true, $5, $6, 3000, 4294967296, 1, 34359738368,
			2000, 2147483648, 1, 21474836480, 1, 'build-v0',
			'sha256:test-certification', now(), now(), now()
)`, workerID, workerID.String(), f.groupID, serviceID, buildPlacementToolchainCatalogDigest, runtimeID)
	} else {
		mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, attestation_fingerprint, state,
    current_epoch, current_service_id, protocol_version, supports_build,
    runtime_identity_id, epoch_started_at
) VALUES (
    $1, $2, $3, 'sha256:test-attestation', 'registering',
    1, $4, 'helmr.worker.v0', true, $5, now()
)`, workerID, workerID.String(), f.groupID, serviceID, runtimeID)
	}
	mustDispatchExec(t, f.ctx, f.pool, `
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    workload_disk_pressure_bps, scratch_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, leaked_slot_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth, observed_at
) VALUES ($1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, now())`, workerID)
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
		name                     string
		certified                bool
		insufficientCertifiedCPU bool
	}{
		{name: "insufficient certified CPU", certified: true, insufficientCertifiedCPU: true},
		{name: "uncertified worker", certified: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBuildPlacementFixture(t)
			workerID := fixture.addWorker(t, test.certified)
			if test.insufficientCertifiedCPU {
				mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET certified_cpu_millis = 2999
 WHERE id = $1`, workerID)
			}
			_, err := fixture.authority.PlaceReadyBuild(
				fixture.ctx, fixture.candidate(),
				pgvalue.Timestamptz(time.Now().UTC().Add(-time.Minute)),
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

func TestPlaceReadyBuildExcludesWorkerWithAnotherToolchainCatalog(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	workerID := fixture.addWorker(t, true)
	mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET toolchain_catalog_digest = decode(repeat('04', 32), 'hex')
 WHERE id = $1`, workerID)

	_, err := fixture.authority.PlaceReadyBuild(
		fixture.ctx, fixture.candidate(),
		pgvalue.Timestamptz(time.Now().UTC().Add(-time.Minute)),
	)
	if !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("PlaceReadyBuild() error = %v, want ErrCapacityUnavailable", err)
	}
}

func TestPlaceReadyBuildUsesCatalogMembershipInsteadOfPinEquality(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	workerID := fixture.addWorker(t, true)
	mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE deployments
   SET build_standard_toolchain_digest = decode(repeat('04', 32), 'hex')
 WHERE id = $1`, fixture.deploymentID)

	lease, err := fixture.authority.PlaceReadyBuild(
		fixture.ctx, fixture.candidate(),
		pgvalue.Timestamptz(time.Now().UTC().Add(-time.Minute)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.WorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("placed worker = %v, want %s", lease.WorkerInstanceID, workerID)
	}
}

func TestPlaceReadyBuildLeavesUnregisteredPinWaiting(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	fixture.addWorker(t, true)
	mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE deployments
   SET build_standard_toolchain_digest = decode(repeat('05', 32), 'hex')
 WHERE id = $1`, fixture.deploymentID)

	_, err := fixture.authority.PlaceReadyBuild(
		fixture.ctx, fixture.candidate(),
		pgvalue.Timestamptz(time.Now().UTC().Add(-time.Minute)),
	)
	if !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("PlaceReadyBuild() error = %v, want ErrCapacityUnavailable", err)
	}
	var leases int
	if err := fixture.pool.QueryRow(
		fixture.ctx,
		`SELECT count(*) FROM deployment_build_leases WHERE deployment_id = $1`,
		fixture.deploymentID,
	).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("created %d build leases for an unregistered toolchain pin", leases)
	}
}

func TestPlaceReadyBuildRequiresV0WorkerRuntimeIdentity(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	workerID := fixture.addWorker(t, true)
	lease, err := fixture.authority.PlaceReadyBuild(
		fixture.ctx, fixture.candidate(),
		pgvalue.Timestamptz(time.Now().UTC().Add(-time.Minute)),
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

func TestPlaceBuildFinalLockRechecksToolchainCatalog(t *testing.T) {
	fixture := newBuildPlacementFixture(t)
	workerID := fixture.addWorker(t, true)
	mustDispatchExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET toolchain_catalog_digest = decode(repeat('04', 32), 'hex')
 WHERE id = $1`, workerID)

	now := time.Now().UTC()
	_, err := fixture.authority.placeBuild(fixture.ctx, placeBuildParams{
		ObservationFreshAfter:           pgvalue.Timestamptz(now.Add(-time.Minute)),
		ExpectedLeaseSequence:           1,
		ExpectedStandardToolchainDigest: bytes.Repeat([]byte{2}, 32),
		Lease: db.LeaseQueuedDeploymentBuildParams{
			OrgID: pgvalue.UUID(fixture.orgID), DeploymentID: pgvalue.UUID(fixture.deploymentID),
			BuildRegionID:      "us-east-1",
			RequestedCpuMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedScratchBytes: 32 << 30, RequestedBuildExecutors: 1,
			LeaseSequence: 1, BuildLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			WorkerGroupID: fixture.groupID, BuildWorkerInstanceID: pgvalue.UUID(workerID),
			WorkerEpoch: 1, WorkerProtocolVersion: "helmr.worker.v0",
			BuildSnapshot:       []byte(`{"source":"test"}`),
			StartDeadlineAt:     pgvalue.Timestamptz(now.Add(time.Minute)),
			BuildLeaseExpiresAt: pgvalue.Timestamptz(now.Add(5 * time.Minute)),
		},
	})
	if !errors.Is(err, pgx.ErrNoRows) || !strings.Contains(err.Error(), "lock eligible worker epoch") {
		t.Fatalf("placeBuild() error = %v, want worker epoch fence rejection", err)
	}
	var leases int
	if err := fixture.pool.QueryRow(fixture.ctx,
		`SELECT count(*) FROM deployment_build_leases WHERE deployment_id = $1`,
		fixture.deploymentID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("created %d build leases after the final toolchain catalog recheck failed", leases)
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

func dispatchPublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	id, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustDispatchExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
