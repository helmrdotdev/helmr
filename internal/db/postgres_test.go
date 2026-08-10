package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresIDs struct {
	orgID                    uuid.UUID
	projectID                uuid.UUID
	environmentID            uuid.UUID
	deploymentID             uuid.UUID
	workspaceImageArtifactID uuid.UUID
}

func seedPostgres(t *testing.T, ctx context.Context, pool *pgxpool.Pool) postgresIDs {
	t.Helper()
	ids := postgresIDs{
		orgID:         dbtest.DefaultOrgID,
		projectID:     uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()),
		deploymentID:  uuid.Must(uuid.NewV7()),
	}
	projectSlug := "project-" + dbtest.ShortID(ids.projectID)
	environmentSlug := "env-" + dbtest.ShortID(ids.environmentID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Project')
	`, ids.projectID, ids.orgID, dbtest.DefaultRegionID, projectSlug)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Environment', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug)
	sourceArtifactID := seedPostgresArtifact(
		t,
		ctx,
		pool,
		ids,
		"deployment_source",
		api.DeploymentSourceArtifactMediaType,
		"source",
	)
	programArtifactID := seedPostgresArtifact(
		t,
		ctx,
		pool,
		ids,
		"deployment_program",
		deployment.ProgramArtifactMediaType,
		"program",
	)
	ids.workspaceImageArtifactID = seedPostgresArtifact(
		t,
		ctx,
		pool,
		ids,
		"workspace_image",
		deployment.WorkspaceImageArtifactMediaType,
		"workspace-image",
	)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, org_id, build_region_id, project_id, environment_id,
			build_node_version, build_runtime_digest, build_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract, image_cache_mode, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_index_digest, queue_config,
			status, deployed_at
		)
		VALUES (
			$1, $2, $3, $4, $5,
			'24.16.0', decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'prefer', 'v1', $6, $7,
			$8, decode(repeat('03', 32), 'hex'),
			'{"formatVersion":0,"queues":[]}'::jsonb, 'deployed', now()
		)
	`, ids.deploymentID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID,
		dbtest.Digest("deployment-"+ids.deploymentID.String()), sourceArtifactID,
		programArtifactID)
	return ids
}

func seedPostgresArtifact(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	ids postgresIDs,
	kind string,
	mediaType string,
	label string,
) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	digest := dbtest.Digest(label + "-" + ids.deploymentID.String())
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, $3)
	`, ids.orgID, digest, mediaType)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES ($1, $2, $3, $4, $5, $6::artifact_kind, 1, $7)
	`, id, ids.orgID, ids.projectID, ids.environmentID, digest, kind, mediaType)
	return id
}

func newPostgresDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
	queries := db.New(pool)
	if _, err := queries.CreateRegion(ctx, db.CreateRegionParams{
		ID: dbtest.DefaultRegionID, DisplayName: dbtest.DefaultRegionDisplay,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: dbtest.DefaultWorkerGroupID, TokenID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		TokenHash: make([]byte, 32), RegionID: dbtest.DefaultRegionID,
		Name: dbtest.DefaultWorkerGroupID, AllowsRun: true, AllowsBuild: true,
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, vm_runtime_contract, vm_runtime_descriptor_digest,
			firecracker_digest, firecracker_version, snapshot_format_version,
			host_kernel_release, cpu_template_kind,
			kernel_digest, initramfs_digest, rootfs_digest
		) VALUES (
			$1, 'x86_64', 'helmr.vm-runtime.v0', $2,
			$3, '1.16.1', '6.0.0', '6.8.0-test', 'none',
			$4, $5, $6
		)
	`, dbtest.DefaultRuntimeID, dbtest.Digest("default-vm-runtime-descriptor"),
		dbtest.Digest("default-firecracker"), dbtest.Digest("default-kernel"),
		dbtest.Digest("default-initramfs"), dbtest.Digest("default-rootfs"))
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_pools (
			id, worker_group_id, name, state, allows_run, allows_build,
			runtime_identity_id, substrate_format, substrate_contract,
			capacity_cpu_millis, capacity_memory_bytes, capacity_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_build_executors, sealed_at
		) VALUES (
			$1, $2, 'default', 'active', true, true,
			$3, 'ext4', 'helmr.substrate.ext4.v0',
			8000, 17179869184, 274877906944,
			4000, 8589934592, 34359738368,
			8, 1, now()
		)
	`, dbtest.DefaultWorkerPoolID, dbtest.DefaultWorkerGroupID, dbtest.DefaultRuntimeID)
	for vcpu := int32(1); vcpu <= 4; vcpu++ {
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO worker_pool_cpu_shapes (worker_pool_id, vcpu_count, cpu_config_digest)
			VALUES ($1, $2, $3)
		`, dbtest.DefaultWorkerPoolID, vcpu, dbtest.DefaultCPUConfigID)
	}
	dbtest.MustExec(t, ctx, pool, `
		UPDATE worker_groups
		   SET primary_run_pool_id = $2, primary_build_pool_id = $2
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID, dbtest.DefaultWorkerPoolID)
	return pool
}
