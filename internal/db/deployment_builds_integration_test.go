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

func TestLeaseQueuedDeploymentBuildDoesNotMutateWrongWorkerGroupDeployment(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	otherWorkerGroupID := placeEnvironmentInOtherWorkerGroup(t, ctx, pool, ids)
	workerID := uuid.Must(uuid.NewV7())
	workerResourceID := "worker-" + shortUUID(workerID)
	runtimeID := "runtime-" + shortUUID(workerID)
	otherDeploymentID := uuid.Must(uuid.NewV7())
	otherArtifactID := uuid.Must(uuid.NewV7())
	otherDigest := testDigest("wrong-worker-group-deployment-source")
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
			id, org_id, worker_group_id, resource_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, 'active', $5,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$6, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultWorkerGroupID, workerResourceID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/json')
	`, ids.orgID, otherDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, otherArtifactID, ids.orgID, ids.projectID, ids.environmentID, otherDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, build_worker_group_id, project_id, environment_id, version,
			content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'wrong-worker-group-build', $6, $7, 'queued')
	`, otherDeploymentID, ids.orgID, otherWorkerGroupID, ids.projectID, ids.environmentID, otherDigest, otherArtifactID, testDeploymentPublicID(t)); err != nil {
		t.Fatal(err)
	}
	_, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		WorkerGroupID:         dbtest.DefaultWorkerGroupID,
		BuildLeaseID:          pgtype.Text{String: "wrong-worker-group-build-lease", Valid: true},
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
		t.Fatalf("wrong-worker-group deployment mutated: status=%s lease=%q", status, leaseID.String)
	}
}

func TestLeaseQueuedDeploymentBuildRejectsDisabledWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)
	workerID := uuid.Must(uuid.NewV7())
	workerResourceID := "worker-" + shortUUID(workerID)
	runtimeID := "runtime-" + shortUUID(workerID)
	deploymentID := uuid.Must(uuid.NewV7())
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("disabled-worker-group-deployment-source")
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
			id, org_id, worker_group_id, resource_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, $3, $4, 'active', $5,
			1000, 1024, 4096, 1, 1000, 1024, 4096, 1,
			$6, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, ids.orgID, dbtest.DefaultWorkerGroupID, workerResourceID, api.CurrentWorkerProtocolVersion, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/json')
	`, ids.orgID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (
			id, public_id, org_id, build_worker_group_id, project_id, environment_id, version,
			content_hash, deployment_source_artifact_id, status
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'disabled-worker-group-build', $6, $7, 'queued')
	`, deploymentID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, digest, artifactID, testDeploymentPublicID(t)); err != nil {
		t.Fatal(err)
	}
	_, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		WorkerGroupID:         dbtest.DefaultWorkerGroupID,
		BuildLeaseID:          pgtype.Text{String: "disabled-worker-group-build-lease", Valid: true},
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
		t.Fatalf("disabled worker group deployment mutated: status=%s lease=%q", status, leaseID.String)
	}
}
