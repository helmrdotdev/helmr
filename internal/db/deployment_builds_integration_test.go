package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLeaseQueuedDeploymentBuildDoesNotMutateWrongCellDeployment(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	otherCellID := routeEnvironmentToOtherCell(t, ctx, pool, ids)
	workerID := uuid.Must(uuid.NewV7())
	workerResourceID := "worker-" + shortUUID(workerID)
	runtimeID := "runtime-" + shortUUID(workerID)
	otherDeploymentID := uuid.Must(uuid.NewV7())
	otherArtifactID := uuid.Must(uuid.NewV7())
	otherDigest := testDigest("wrong-cell-deployment-source")
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
	if err := queries.EnsureRuntimeReleaseSelection(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, org_id, cell_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$7, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultCellID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
		VALUES ($1, $2, $3, 1, 'application/json')
	`, ids.orgID, otherCellID, otherDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, 'task_bundle', 1, 'application/json')
	`, otherArtifactID, ids.orgID, otherCellID, ids.projectID, ids.environmentID, otherDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, cell_id, project_id, environment_id, worker_group_id, version,
			content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, 'wrong-cell-build', $7, $8, 'queued')
	`, otherDeploymentID, ids.orgID, otherCellID, ids.projectID, ids.environmentID, workerGroupID, otherDigest, otherArtifactID, testDeploymentPublicID(t)); err != nil {
		t.Fatal(err)
	}
	_, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		CellID:                dbtest.DefaultCellID,
		WorkerGroupID:         pgvalue.UUID(workerGroupID),
		BuildLeaseID:          pgtype.Text{String: "wrong-cell-build-lease", Valid: true},
		BuildWorkerInstanceID: pgvalue.UUID(workerID),
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseQueuedDeploymentBuild err = %v, want no rows", err)
	}
	var status db.DeploymentStatus
	var leaseID pgtype.Text
	if err := pool.QueryRow(ctx, `
		SELECT status, build_lease_id
		  FROM deployments
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, otherDeploymentID).Scan(&status, &leaseID); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusQueued || leaseID.Valid {
		t.Fatalf("wrong-cell deployment mutated: status=%s lease=%q", status, leaseID.String)
	}
}

func TestLeaseQueuedDeploymentBuildRejectsDisabledEnvironmentRoute(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	disableDefaultEnvironmentRoute(t, ctx, pool, ids)
	workerID := uuid.Must(uuid.NewV7())
	workerResourceID := "worker-" + shortUUID(workerID)
	runtimeID := "runtime-" + shortUUID(workerID)
	deploymentID := uuid.Must(uuid.NewV7())
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("stale-route-deployment-source")
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
	if err := queries.EnsureRuntimeReleaseSelection(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, org_id, cell_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, $5, 'active', $6,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$7, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultCellID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
		VALUES ($1, $2, $3, 1, 'application/json')
	`, ids.orgID, dbtest.DefaultCellID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, 'task_bundle', 1, 'application/json')
	`, artifactID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, cell_id, project_id, environment_id, worker_group_id, version,
			content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $9, $2, $3, $4, $5, $6, 'stale-route-build', $7, $8, 'queued')
	`, deploymentID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, workerGroupID, digest, artifactID, testDeploymentPublicID(t)); err != nil {
		t.Fatal(err)
	}
	_, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		CellID:                dbtest.DefaultCellID,
		WorkerGroupID:         pgvalue.UUID(workerGroupID),
		BuildLeaseID:          pgtype.Text{String: "disabled-route-build-lease", Valid: true},
		BuildWorkerInstanceID: pgvalue.UUID(workerID),
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaseQueuedDeploymentBuild err = %v, want no rows", err)
	}
	var status db.DeploymentStatus
	var leaseID pgtype.Text
	if err := pool.QueryRow(ctx, `
		SELECT status, build_lease_id
		  FROM deployments
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, deploymentID).Scan(&status, &leaseID); err != nil {
		t.Fatal(err)
	}
	if status != db.DeploymentStatusQueued || leaseID.Valid {
		t.Fatalf("disabled-route deployment mutated: status=%s lease=%q", status, leaseID.String)
	}
}
