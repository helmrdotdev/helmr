package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestGetWorkerRunWaitScopeUsesWorkerGroupIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)

	scope, err := queries.GetWorkerRunWaitScope(ctx, db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pgvalue.MustUUIDValue(scope.RunID), ids.runID; got != want {
		t.Fatalf("run id = %s, want %s", got, want)
	}
	if got, want := pgvalue.MustUUIDValue(scope.CurrentRunLeaseID), runLeaseID; got != want {
		t.Fatalf("run lease id = %s, want %s", got, want)
	}
	if got := scope.WorkerCniProfile; got != "default" {
		t.Fatalf("worker cni profile = %q, want default", got)
	}
	if !scope.WorkspaceMountID.Valid {
		t.Fatal("workspaceMount id is invalid")
	}
}

func TestGetWorkerRunWaitScopeRejectsDisabledWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)

	_, err := queries.GetWorkerRunWaitScope(ctx, db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorkerRunWaitScope disabled worker group error = %v, want pgx.ErrNoRows", err)
	}
}

func ensureRunningWorkspaceMount(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	workspaceMountID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ('test-runtime', 'arm64', 'test-abi', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'test-cni')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		WITH existing AS (
			SELECT id
			  FROM workspace_mounts
			 WHERE org_id = $2
			   AND workspace_id = $3
			   AND state = 'mounted'
			 LIMIT 1
		),
		inserted AS (
			INSERT INTO workspace_mounts (
				id, org_id, worker_group_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
				image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
				workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
				workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type,
				workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state
			)
			SELECT $1, workspaces.org_id, workspaces.worker_group_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
			       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
			       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
			       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
			       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
			       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
			       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
			       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted'
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
		   AND NOT EXISTS (SELECT 1 FROM existing)
		RETURNING id
		)
		SELECT id FROM inserted
		UNION ALL
		SELECT id FROM existing
		LIMIT 1
	`, workspaceMountID, ids.orgID, ids.workspaceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
}

func seedActiveWorkspaceLeaseForRun(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	ensureRunningWorkspaceMount(t, ctx, pool, ids)
	leaseID := uuid.Must(uuid.NewV7())
	var workspaceMountID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_mounts
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'mounted'
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, worker_group_id, project_id, environment_id, id, $2,
		       'write', 'active', $3, current_version_id, current_version_id,
		       1, 'test-fencing-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, leaseID, workspaceMountID, ids.runID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
}
