package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
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
	taskBundleDigest := testDigest("task-bundle")
	projectSlug := "project-" + shortUUID(ids.projectID)
	environmentSlug := "env-" + shortUUID(ids.environmentID)
	var workerGroupID uuid.UUID
	if _, err := pool.Exec(ctx, `
		INSERT INTO organizations (id, name, slug) VALUES ($1, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO projects (id, org_id, slug, name) VALUES ($1, $2, $3, 'Project')
	`, ids.projectID, ids.orgID, projectSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Env', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug); err != nil {
		t.Fatal(err)
	}
	imageArtifactID, imageDigest := seedSandboxImageArtifact(t, ctx, pool, ids)
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 1, 'application/json')
	`, taskBundleDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, taskBundleArtifactID, ids.orgID, ids.projectID, ids.environmentID, taskBundleDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (id, org_id, project_id, environment_id, worker_group_id, version, content_hash, deployment_source_artifact_id, status)
		VALUES ($1, $2, $3, $4, $5, 'v1', $6, $7, 'deployed')
	`, ids.deploymentID, ids.orgID, ids.projectID, ids.environmentID, workerGroupID, taskBundleDigest, taskBundleArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			contract_version, fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'default', $6, 'oci-tar', 'sha256:rootfs', $7, 'oci-tar', '/workspace',
			'test', 'guestd-test', 'adapter-test', 'tar', 1, 'sandbox-fingerprint')
	`, ids.deploymentSandboxID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, imageArtifactID, imageDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_tasks (
			id, org_id, project_id, environment_id, deployment_id, deployment_sandbox_id, task_id, bundle_artifact_id,
			queue_name, max_active_duration_ms
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval-task', $7, 'default', 300000)
	`, ids.taskID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.deploymentSandboxID, taskBundleArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspaces (
			id, org_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'default', 'sandbox-fingerprint')
	`, ids.workspaceID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID); err != nil {
		t.Fatal(err)
	}
	initialWorkspaceArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	initialVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, $6, 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $7
	`, initialVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, workspace.ArtifactEncoding, initialWorkspaceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, initialVersionID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	taskSessionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			task_session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, ids.runID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, ids.runID, ids.orgID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	return ids
}

func seedTaskSessionForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	var taskSessionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT task_session_id
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&taskSessionID); err != nil {
		t.Fatal(err)
	}
	return taskSessionID
}

func seedRunningTaskSessionLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (taskSessionID uuid.UUID, runLeaseID uuid.UUID, workerID uuid.UUID) {
	t.Helper()
	taskSessionID = seedTaskSessionForRun(t, ctx, pool, ids)
	runLeaseID = uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID = uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	dispatchMessageID := "dispatch-" + runLeaseID.String()[:8]
	dispatchLeaseID := "lease-" + runLeaseID.String()[:8]
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			worker_group_id, protocol_version,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, 1000, 1024, 4096, $3, $4, 1, 1000, 1024, 4096, 1,
			$5, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'running')
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			1, 'running', now() + interval '1 hour', $9,
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, ids.runID, attemptID, workerID, workerGroupID, dispatchMessageID, dispatchLeaseID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET task_session_id = $1,
		       workspace_id = $6,
		       current_run_lease_id = $2,
		       current_attempt_id = $3,
		       current_attempt_number = 1,
		       status = 'running',
		       execution_status = 'executing',
		       active_started_at = now()
		 WHERE org_id = $4
		   AND id = $5
	`, taskSessionID, runLeaseID, attemptID, ids.orgID, ids.runID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1000, 1024, 4096, 1, $3, 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, 'reserved', 'default', $3, $4, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	return taskSessionID, runLeaseID, workerID
}

func requestWorkspaceMaterializationForTest(ctx context.Context, queries *db.Queries, arg db.EnsureWorkspaceMaterializationRequestedParams) (db.EnsureWorkspaceMaterializationRequestedRow, error) {
	return queries.EnsureWorkspaceMaterializationRequested(ctx, arg)
}

func seedWorkspaceVersionArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("workspace-version-" + artifactID.String())
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 10, $2)
	`, digest, workspace.ArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'workspace_version', 10, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, workspace.ArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	return artifactID
}

func seedSandboxImageArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (uuid.UUID, string) {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("sandbox-image-" + artifactID.String())
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 6, $2)
	`, digest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'sandbox_image', 6, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
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

func canonicalFingerprint(t *testing.T, data []byte) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
