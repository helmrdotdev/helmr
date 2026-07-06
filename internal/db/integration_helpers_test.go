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
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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

func testDeploymentPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Deployment)
}

func testEnvironmentPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Environment)
}

func testTaskPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Task)
}

func testSandboxPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Sandbox)
}

func testSchedulePublicID(t *testing.T) string {
	return testPublicID(t, publicid.Schedule)
}

func testWorkspacePublicID(t *testing.T) string {
	return testPublicID(t, publicid.Workspace)
}

func testWorkspaceVersionPublicID(t *testing.T) string {
	return testPublicID(t, publicid.WorkspaceVersion)
}

func testSessionRunPublicID(t *testing.T) string {
	return testPublicID(t, publicid.SessionRun)
}

func testRunPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Run)
}

func testRunOperationPublicID(t *testing.T) string {
	return testPublicID(t, publicid.RunOperation)
}

func testWaitPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Wait)
}

func testStreamPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Stream)
}

func testStreamRecordPublicID(t *testing.T) string {
	return testPublicID(t, publicid.StreamRecord)
}

func testTokenPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Token)
}

func testPublicAccessTokenPublicID(t *testing.T) string {
	return testPublicID(t, publicid.PublicAccessToken)
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
	if _, err := pool.Exec(ctx, `
		INSERT INTO organizations (id, public_id, name, slug) VALUES ($1, $2, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID, testPublicID(t, publicid.Organization)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
			VALUES ($1, $5::text, $2, $3::text, $4::text, 'Project')
	`, ids.projectID, ids.orgID, dbtest.DefaultRegionID, projectSlug, testPublicID(t, publicid.Project)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
			VALUES ($1, $5::text, $2, $3, $4::text, 'Env', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug, testPublicID(t, publicid.Environment)); err != nil {
		t.Fatal(err)
	}
	imageArtifactID, imageDigest := seedSandboxImageArtifact(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/json')
	`, ids.orgID, taskBundleDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, taskBundleArtifactID, ids.orgID, ids.projectID, ids.environmentID, taskBundleDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (id, public_id, org_id, build_worker_group_id, project_id, environment_id, version, content_hash, deployment_source_artifact_id, status)
		VALUES ($1, $8, $2, $3, $4, $5, 'v1', $6, $7, 'deployed')
	`, ids.deploymentID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, taskBundleDigest, taskBundleArtifactID, testPublicID(t, publicid.Deployment)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_queues (org_id, project_id, environment_id, deployment_id, name)
		VALUES ($1, $2, $3, $4, 'default')
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, public_id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			disk_floor_mib, contract_version, fingerprint
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'default', $6, 'oci-tar', 'sha256:rootfs', $7, 'oci-tar', '/workspace',
			'test', 'guestd-test', 'adapter-test', 'tar', 1024, 1, 'sandbox-fingerprint')
	`, ids.deploymentSandboxID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, imageArtifactID, imageDigest, testPublicID(t, publicid.Sandbox)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (public_id, org_id, project_id, environment_id, task_id)
		VALUES ($5, $1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID, testPublicID(t, publicid.Task)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_tasks (
			id, public_id, org_id, project_id, environment_id, deployment_id, deployment_sandbox_id, task_id, bundle_artifact_id,
			queue_name, max_active_duration_ms
		)
		VALUES ($1, $8, $2, $3, $4, $5, $6, 'approval-task', $7, 'default', 300000)
	`, ids.taskID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.deploymentSandboxID, taskBundleArtifactID, testPublicID(t, publicid.DeploymentTask)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspaces (
			id, public_id, org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint
		)
		VALUES ($1, $7, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint')
	`, ids.workspaceID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, testPublicID(t, publicid.Workspace)); err != nil {
		t.Fatal(err)
	}
	initialWorkspaceArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	initialVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $8, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, $6, 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $7
	`, initialVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, workspace.ArtifactEncoding, initialWorkspaceArtifactID, testPublicID(t, publicid.WorkspaceVersion)); err != nil {
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
	sessionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (
			id, public_id, org_id, worker_group_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'approval-task', $6, $6, $7)
	`, sessionID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID, testPublicID(t, publicid.Session)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, public_id, org_id, worker_group_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID, testPublicID(t, publicid.Run)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, ids.runID, ids.orgID, sessionID); err != nil {
		t.Fatal(err)
	}
	return ids
}

func seedSessionForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	var sessionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT session_id
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func seedRunningSessionLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (sessionID uuid.UUID, runLeaseID uuid.UUID, workerID uuid.UUID) {
	t.Helper()
	sessionID = seedSessionForRun(t, ctx, pool, ids)
	runLeaseID = uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID = uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	dispatchMessageID := "dispatch-" + runLeaseID.String()[:8]
	dispatchLeaseID := "lease-" + runLeaseID.String()[:8]
	workspaceMountID := uuid.Must(uuid.NewV7())
	runtimeInstanceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, worker_group_id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			protocol_version, total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, 1000, 1024, 4096, $4, 1, 1000, 1024, 4096, 1,
			$5, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, dbtest.DefaultWorkerGroupID, workerResourceID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, worker_group_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, $4, 1, 'running')
	`, attemptID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, worker_group_id, run_id, attempt_id, worker_instance_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			1, 'running', now() + interval '1 hour', $9,
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.runID, attemptID, workerID, dispatchMessageID, dispatchLeaseID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format, workspace_artifact_id,
			workspace_artifact_encoding, workspace_artifact_entry_count, workspace_artifact_digest,
			workspace_artifact_size_bytes, workspace_artifact_media_type, workspace_mount_path,
			runtime_abi, guestd_abi, adapter_abi, state, mounted_at, last_heartbeat_at
		)
		SELECT $1, workspaces.org_id, workspaces.worker_group_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
		       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
		       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
		       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
		       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
		       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted', now(), now()
		  FROM workspaces
		  JOIN deployment_sandboxes
		    ON deployment_sandboxes.org_id = workspaces.org_id
		   AND deployment_sandboxes.project_id = workspaces.project_id
		   AND deployment_sandboxes.environment_id = workspaces.environment_id
		   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
		  JOIN artifacts AS image_artifact
		    ON image_artifact.org_id = deployment_sandboxes.org_id
		   AND image_artifact.project_id = deployment_sandboxes.project_id
		   AND image_artifact.environment_id = deployment_sandboxes.environment_id
		   AND image_artifact.id = deployment_sandboxes.image_artifact_id
		  JOIN workspace_versions
		    ON workspace_versions.org_id = workspaces.org_id
		   AND workspace_versions.project_id = workspaces.project_id
		   AND workspace_versions.environment_id = workspaces.environment_id
		   AND workspace_versions.workspace_id = workspaces.id
		   AND workspace_versions.id = workspaces.current_version_id
		  JOIN artifacts AS workspace_artifact
		    ON workspace_artifact.org_id = workspace_versions.org_id
		   AND workspace_artifact.project_id = workspace_versions.project_id
		   AND workspace_artifact.environment_id = workspace_versions.environment_id
		   AND workspace_artifact.id = workspace_versions.artifact_id
		 WHERE workspaces.org_id = $2
		   AND workspaces.id = $3
	`, workspaceMountID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, worker_instance_id,
			runtime_release_id, deployment_sandbox_id, runtime_key_hash, runtime_key,
			sandbox_fingerprint, rootfs_digest, image_digest, image_format,
			sandbox_image_artifact_id, sandbox_image_artifact_digest,
			sandbox_image_artifact_format, workspace_mount_path, runtime_abi,
			guestd_abi, adapter_abi, network_policy, reserved_cpu_millis,
			reserved_memory_mib, reserved_disk_mib, reserved_execution_slots,
			workspace_mount_id, owner_run_id, owner_run_lease_id,
			owner_run_state_version, state, instance_token, last_heartbeat_at,
			running_at
		)
		SELECT $1, workspace_mounts.org_id, workspace_mounts.worker_group_id, workspace_mounts.project_id,
		       workspace_mounts.environment_id, $2, $3,
		       workspace_mounts.deployment_sandbox_id, $4, '{}'::jsonb,
		       workspace_mounts.sandbox_fingerprint, workspace_mounts.rootfs_digest,
		       workspace_mounts.image_digest, workspace_mounts.image_format,
		       workspace_mounts.image_artifact_id, image_artifact.digest,
		       workspace_mounts.image_artifact_format, workspace_mounts.workspace_mount_path,
		       workspace_mounts.runtime_abi, workspace_mounts.guestd_abi,
		       workspace_mounts.adapter_abi, '{}'::jsonb,
		       1000, 1024, 4096, 1,
		       workspace_mounts.id, $5, $6, 0, 'running',
		       'runtime-instance-token-' || $7, now(), now()
		  FROM workspace_mounts
		  JOIN artifacts AS image_artifact
		    ON image_artifact.org_id = workspace_mounts.org_id
		   AND image_artifact.project_id = workspace_mounts.project_id
		   AND image_artifact.environment_id = workspace_mounts.environment_id
		   AND image_artifact.id = workspace_mounts.image_artifact_id
		 WHERE workspace_mounts.org_id = $8
		   AND workspace_mounts.id = $9
	`, runtimeInstanceID, workerID, runtimeID, "runtime-key-"+shortUUID(runtimeInstanceID), ids.runID, runLeaseID, shortUUID(runtimeInstanceID), ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET runtime_instance_id = $1,
		       updated_at = now()
		 WHERE org_id = $2
		   AND id = $3
	`, runtimeInstanceID, ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET session_id = $1,
		       workspace_id = $6,
		       workspace_mount_id = $7,
		       current_run_lease_id = $2,
		       current_attempt_id = $3,
		       current_attempt_number = 1,
		       status = 'running',
		       execution_status = 'executing',
		       active_started_at = now()
		 WHERE org_id = $4
		   AND id = $5
	`, sessionID, runLeaseID, attemptID, ids.orgID, ids.runID, ids.workspaceID, workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_snapshots (
			org_id, worker_group_id, run_id, version, status, execution_status,
			attempt_id, run_lease_id, transition, reason
		)
		SELECT org_id, worker_group_id, id, state_version, status, execution_status,
		       current_attempt_id, current_run_lease_id, 'run.started', '{}'::jsonb
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO run_runtime_requirements (
				run_id, org_id, worker_group_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
				requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
				initramfs_digest, rootfs_digest, cni_profile
			)
			VALUES ($1, $2, $3, 1000, 1024, 4096, 1, $4, 'arm64', 'test', 'sha256:kernel',
				'sha256:initramfs', 'sha256:rootfs', 'default')
		`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, worker_group_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, $3, 'reserved', 'default', $4, $5, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dbtest.DefaultWorkerGroupID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	return sessionID, runLeaseID, workerID
}

func requestWorkspaceMountForTest(ctx context.Context, queries *db.Queries, arg db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error) {
	return queries.EnsureWorkspaceMountRequested(ctx, arg)
}

func seedRuntimeSubstrateSourceInOtherWorkerGroup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, label string) (workerGroupID string, deploymentSandboxID uuid.UUID) {
	t.Helper()
	workerGroupID = placeEnvironmentInOtherWorkerGroup(t, ctx, pool, ids)
	deploymentID := uuid.Must(uuid.NewV7())
	deploymentSandboxID = uuid.Must(uuid.NewV7())
	taskBundleArtifactID := uuid.Must(uuid.NewV7())
	imageArtifactID := uuid.Must(uuid.NewV7())
	taskBundleDigest := testDigest(label + "-task-bundle")
	imageDigest := testDigest(label + "-sandbox-image")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/json'),
		       ($1, $3, 6, $4)
	`, ids.orgID, taskBundleDigest, imageDigest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json'),
		       ($6, $2, $3, $4, $7, 'sandbox_image', 6, $8)
	`, taskBundleArtifactID, ids.orgID, ids.projectID, ids.environmentID, taskBundleDigest, imageArtifactID, imageDigest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, build_worker_group_id, project_id, environment_id,
			version, content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, $7, $8, 'deployed')
	`, deploymentID, ids.orgID, workerGroupID, ids.projectID, ids.environmentID, "wrong-worker-group-"+shortUUID(deploymentID), taskBundleDigest, taskBundleArtifactID, testPublicID(t, publicid.Deployment)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, public_id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			disk_floor_mib, contract_version, fingerprint
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'wrong-worker-group', $6, 'oci-tar', 'sha256:rootfs', $7, 'oci-tar', '/workspace',
			'test', 'guestd-test', 'adapter-test', 'tar', 1024, 1, $8)
	`, deploymentSandboxID, ids.orgID, ids.projectID, ids.environmentID, deploymentID, imageArtifactID, imageDigest, "wrong-worker-group-"+shortUUID(deploymentSandboxID), testPublicID(t, publicid.Sandbox)); err != nil {
		t.Fatal(err)
	}
	return workerGroupID, deploymentSandboxID
}

func seedWorkspaceVersionArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("workspace-version-" + artifactID.String())
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 10, $3)
	`, ids.orgID, digest, workspace.ArtifactMediaType); err != nil {
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
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 6, $3)
	`, ids.orgID, digest, api.SandboxImageArtifactMediaType); err != nil {
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
	if err := workergroup.Bootstrap(ctx, db.New(pool), workergroup.BootstrapConfig{
		RegionID:          dbtest.DefaultRegionID,
		DefaultRegionID:   dbtest.DefaultRegionID,
		Provider:          dbtest.DefaultProvider,
		ProviderRegion:    dbtest.DefaultProviderRegion,
		RegionDisplayName: dbtest.DefaultRegionDisplay,
		WorkerGroupID:     dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := workergroup.ReportHealth(ctx, db.New(pool), workergroup.HealthConfig{
		WorkerGroupID:      dbtest.DefaultWorkerGroupID,
		Component:          workergroup.ComponentDispatcher,
		RequiredComponents: workergroup.RoutingRequiredComponents(),
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

func markEnvironmentRouteDrainingWithStaleHealth(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET state = 'draining'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET health_state = 'unhealthy',
		       routing_fresh_until = now() - interval '1 second'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
}
