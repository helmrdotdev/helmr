package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresIDs struct {
	orgID                    uuid.UUID
	projectID                uuid.UUID
	environmentID            uuid.UUID
	deploymentID             uuid.UUID
	workspaceImageArtifactID uuid.UUID
}

func shortUUID(id uuid.UUID) string {
	compact := strings.ReplaceAll(id.String(), "-", "")
	return compact[len(compact)-12:]
}

func seedPostgres(t *testing.T, ctx context.Context, pool *pgxpool.Pool) postgresIDs {
	t.Helper()
	ids := postgresIDs{
		orgID:         dbtest.DefaultOrgID,
		projectID:     uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()),
		deploymentID:  uuid.Must(uuid.NewV7()),
	}
	projectSlug := "project-" + shortUUID(ids.projectID)
	environmentSlug := "env-" + shortUUID(ids.environmentID)
	mustExec(t, ctx, pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID)
	mustExec(t, ctx, pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Project')
	`, ids.projectID, ids.orgID, dbtest.DefaultRegionID, projectSlug)
	mustExec(t, ctx, pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Environment', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug)
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
			rootfs_digest, network_abi
		) VALUES (
			'test-runtime', 'x86_64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default'
		)
		ON CONFLICT DO NOTHING
	`)

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
	mustExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, org_id, build_region_id, project_id, environment_id,
			build_node_version, build_runtime_digest, build_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, image_cache_mode, version, content_hash, deployment_source_artifact_id,
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
		testDigest("deployment-"+ids.deploymentID.String()), sourceArtifactID,
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
	digest := testDigest(label + "-" + ids.deploymentID.String())
	mustExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, $3)
	`, ids.orgID, digest, mediaType)
	mustExec(t, ctx, pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES ($1, $2, $3, $4, $5, $6::artifact_kind, 1, $7)
	`, id, ids.orgID, ids.projectID, ids.environmentID, digest, kind, mediaType)
	return id
}

func mustExec(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	query string,
	args ...any,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func testDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newPostgresDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
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
		ID:                              dbtest.DefaultWorkerGroupID,
		RegionID:                        dbtest.DefaultRegionID,
		Name:                            dbtest.DefaultWorkerGroupID,
		ObservationTtlSeconds:           120,
		AllowsRun:                       true,
		AllowsBuild:                     true,
		RequiredCpuMillis:               1,
		RequiredMemoryBytes:             1,
		RequiredGuestEphemeralDiskBytes: 1,
		RequiredVmSlots:                 1,
		RequiredBuildExecutors:          1,
		ProtocolVersion:                 api.CurrentWorkerProtocolVersion,
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}
