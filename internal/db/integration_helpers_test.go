package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type integrationIDs struct {
	orgID               uuid.UUID
	projectID           uuid.UUID
	environmentID       uuid.UUID
	deploymentID        uuid.UUID
	deploymentSandboxID uuid.UUID
	workspaceID         uuid.UUID
	taskID              uuid.UUID
	runID               uuid.UUID
}

func shortUUID(id uuid.UUID) string {
	compact := strings.ReplaceAll(id.String(), "-", "")
	return compact[len(compact)-12:]
}

func testPublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	id, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testEnvironmentPublicID(t *testing.T) string { return testPublicID(t, publicid.Environment) }
func testTaskPublicID(t *testing.T) string        { return testPublicID(t, publicid.Task) }
func testSchedulePublicID(t *testing.T) string    { return testPublicID(t, publicid.Schedule) }
func testWorkspacePublicID(t *testing.T) string   { return testPublicID(t, publicid.Workspace) }
func testWorkspaceVersionPublicID(t *testing.T) string {
	return testPublicID(t, publicid.WorkspaceVersion)
}

func seedIntegration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) integrationIDs {
	t.Helper()
	ids := integrationIDs{
		orgID:               dbtest.DefaultOrgID,
		projectID:           uuid.Must(uuid.NewV7()),
		environmentID:       uuid.Must(uuid.NewV7()),
		deploymentID:        uuid.Must(uuid.NewV7()),
		deploymentSandboxID: uuid.Must(uuid.NewV7()),
		workspaceID:         uuid.Must(uuid.NewV7()),
		taskID:              uuid.Must(uuid.NewV7()),
		runID:               uuid.Must(uuid.NewV7()),
	}
	taskBundleArtifactID := uuid.Must(uuid.NewV7())
	taskBundleDigest := testDigest("task-bundle-" + ids.deploymentID.String())
	projectSlug := "project-" + shortUUID(ids.projectID)
	environmentSlug := "env-" + shortUUID(ids.environmentID)

	mustExec(t, ctx, pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID, testPublicID(t, publicid.Organization))
	mustExec(t, ctx, pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $5, $2, $3, $4, 'Project')
	`, ids.projectID, ids.orgID, dbtest.DefaultRegionID, projectSlug, testPublicID(t, publicid.Project))
	mustExec(t, ctx, pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $5, $2, $3, $4, 'Env', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug, testEnvironmentPublicID(t))

	imageArtifactID, imageDigest := seedSandboxImageArtifact(t, ctx, pool, ids)
	mustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/json')
	`, ids.orgID, taskBundleDigest)
	mustExec(t, ctx, pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, taskBundleArtifactID, ids.orgID, ids.projectID, ids.environmentID, taskBundleDigest)
	mustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, public_id, org_id, build_region_id, project_id, environment_id,
			version, content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'v1', $6, $7, 'deployed')
	`, ids.deploymentID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID,
		taskBundleDigest, taskBundleArtifactID, testPublicID(t, publicid.Deployment))
	mustExec(t, ctx, pool, `
		INSERT INTO deployment_queues (org_id, project_id, environment_id, deployment_id, name)
		VALUES ($1, $2, $3, $4, 'default')
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID)
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ('test-runtime', 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
		ON CONFLICT DO NOTHING
	`)
	mustExec(t, ctx, pool, `
		INSERT INTO deployment_sandboxes (
			id, public_id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			disk_floor_mib, contract_version, fingerprint
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'default', $6, 'oci-tar', 'sha256:rootfs', $7,
			'oci-tar', '/workspace', 'test', 'guestd-test', 'adapter-test', 'tar', 1024, 1,
			'sandbox-fingerprint')
	`, ids.deploymentSandboxID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID,
		imageArtifactID, imageDigest, testPublicID(t, publicid.Sandbox))
	mustExec(t, ctx, pool, `
		INSERT INTO tasks (public_id, org_id, project_id, environment_id, task_id)
		VALUES ($4, $1, $2, $3, 'approval-task')
	`, ids.orgID, ids.projectID, ids.environmentID, testTaskPublicID(t))
	mustExec(t, ctx, pool, `
		INSERT INTO deployment_tasks (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_sandbox_id, task_id, bundle_artifact_id, queue_name, max_active_duration_ms
		)
		VALUES ($1, $8, $2, $3, $4, $5, $6, 'approval-task', $7, 'default', 300000)
	`, ids.taskID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID,
		ids.deploymentSandboxID, taskBundleArtifactID, testPublicID(t, publicid.DeploymentTask))
	mustExec(t, ctx, pool, `
		INSERT INTO workspaces (
			id, public_id, org_id, region_id, project_id, environment_id,
			deployment_sandbox_id, sandbox_id, sandbox_fingerprint
		)
		VALUES ($1, $7, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint')
	`, ids.workspaceID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID,
		ids.deploymentSandboxID, testWorkspacePublicID(t))

	initialWorkspaceArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	initialVersionID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $8, $2, $3, $4, $5, 'system', 'ready', artifacts.id, $6, 0,
		       artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2 AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4 AND artifacts.id = $7
	`, initialVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID,
		workspace.ArtifactEncoding, initialWorkspaceArtifactID, testWorkspaceVersionPublicID(t))
	mustExec(t, ctx, pool, `
		UPDATE workspaces SET current_version_id = $1 WHERE org_id = $2 AND id = $3
	`, initialVersionID, ids.orgID, ids.workspaceID)

	sessionID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO sessions (
			id, public_id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id
		)
		VALUES ($1, $7, $2, $3, $4, 'approval-task', $5, $5, $6)
	`, sessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID,
		ids.workspaceID, testPublicID(t, publicid.Session))
	mustExec(t, ctx, pool, `
		INSERT INTO runs (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_task_id, workspace_id, task_id, session_id, status, execution_status,
			payload, queue_name, requested_milli_cpu, requested_memory_mib,
			requested_disk_mib, requested_execution_slots, runtime_identity_id, runtime_arch,
			runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile,
			max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting',
			'{}', 'default', 1000, 1024, 4096, 1, 'test-runtime', 'arm64', 'test',
			'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, ids.runID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID,
		ids.taskID, ids.workspaceID, sessionID, testPublicID(t, publicid.Run))
	mustExec(t, ctx, pool, `
		UPDATE sessions SET current_run_id = $1 WHERE org_id = $2 AND id = $3
	`, ids.runID, ids.orgID, sessionID)
	return ids
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedWorkspaceVersionArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("workspace-version-" + artifactID.String())
	mustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1, $2, 10, $3)
	`, ids.orgID, digest, workspace.ArtifactMediaType)
	mustExec(t, ctx, pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'workspace_version', 10, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, workspace.ArtifactMediaType)
	return artifactID
}

func seedSandboxImageArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (uuid.UUID, string) {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("sandbox-image-" + artifactID.String())
	mustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1, $2, 6, $3)
	`, ids.orgID, digest, api.SandboxImageArtifactMediaType)
	mustExec(t, ctx, pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'sandbox_image', 6, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, api.SandboxImageArtifactMediaType)
	return artifactID, digest
}

func testDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newIntegrationDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is not set")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var serverVersion int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		admin.Close()
		t.Skipf("Postgres %d does not provide uuidv7(); skipping integration test", serverVersion)
	}
	name := "helmr_db_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	testDSN := databaseDSN(t, dsn, name)
	if err := schema.Up(ctx, testDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	queries := db.New(pool)
	if err := region.Ensure(ctx, queries, region.BootstrapConfig{
		RegionID:          dbtest.DefaultRegionID,
		DefaultRegionID:   dbtest.DefaultRegionID,
		Provider:          dbtest.DefaultProvider,
		ProviderRegion:    dbtest.DefaultProviderRegion,
		RegionDisplayName: dbtest.DefaultRegionDisplay,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: dbtest.DefaultWorkerGroupID, RegionID: dbtest.DefaultRegionID, Name: dbtest.DefaultWorkerGroupID,
		EnrollmentPolicyFingerprint: "sha256:test-worker-group", AllowsRun: true, AllowsBuild: true,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1, RequiredBuildExecutors: 1,
		ProtocolVersion: api.CurrentWorkerProtocolVersion, AllowedAttestationFingerprints: []string{"sha256:test-attestation"},
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

func databaseDSN(t *testing.T, dsn string, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}
