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
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type integrationIDs struct {
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

func testPublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	id, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testEnvironmentPublicID(t *testing.T) string {
	return testPublicID(t, publicid.Environment)
}

func testWorkspacePublicID(t *testing.T) string {
	return testPublicID(t, publicid.Workspace)
}

func testWorkspaceVersionPublicID(t *testing.T) string {
	return testPublicID(t, publicid.WorkspaceVersion)
}

func seedIntegration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) integrationIDs {
	t.Helper()
	ids := integrationIDs{
		orgID:         dbtest.DefaultOrgID,
		projectID:     uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()),
		deploymentID:  uuid.Must(uuid.NewV7()),
	}
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
		VALUES ($1, $5, $2, $3, $4, 'Environment', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug, testEnvironmentPublicID(t))
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
			rootfs_digest, cni_profile
		) VALUES (
			'test-runtime', 'x86_64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default'
		)
		ON CONFLICT DO NOTHING
	`)

	sourceArtifactID := seedIntegrationArtifact(
		t,
		ctx,
		pool,
		ids,
		"deployment_source",
		api.DeploymentSourceArtifactMediaType,
		"source",
	)
	programArtifactID := seedIntegrationArtifact(
		t,
		ctx,
		pool,
		ids,
		"deployment_program",
		deployment.ProgramArtifactMediaType,
		"program",
	)
	sourceDigest := testDigest("source-" + ids.deploymentID.String())
	programDigest := testDigest("program-" + ids.deploymentID.String())
	programReceipt := dbtest.ProgramReceipt(dbtest.ProgramReceiptAuthority{
		Architecture:            "x86_64",
		ProgramArtifactID:       programArtifactID,
		ProgramDigest:           programDigest,
		ProgramSizeBytes:        1,
		RuntimeDigest:           "sha256:" + strings.Repeat("01", 32),
		SourceArtifactID:        sourceArtifactID,
		SourceDigest:            sourceDigest,
		SourceSizeBytes:         1,
		StandardToolchainDigest: "sha256:" + strings.Repeat("02", 32),
	})
	ids.workspaceImageArtifactID = seedIntegrationArtifact(
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
			id, public_id, org_id, build_region_id, project_id, environment_id,
			build_architecture, build_runtime_digest, build_standard_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_runtime_digest, program_architecture,
			program_receipt, queue_config,
			status, deployed_at
		)
		VALUES (
			$1, $8, $2, $3, $4, $5,
			'x86_64', decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'bun', '1.2.3', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'v1', $6, $7,
			$9, decode(repeat('01', 32), 'hex'), 'x86_64', $10::jsonb,
			'{"formatVersion":0,"queues":[]}'::jsonb, 'deployed', now()
		)
	`, ids.deploymentID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID,
		testDigest("deployment-"+ids.deploymentID.String()), sourceArtifactID,
		testPublicID(t, publicid.Deployment), programArtifactID, programReceipt)
	return ids
}

func seedIntegrationArtifact(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	ids integrationIDs,
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
	name := "helmr_db_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(
			context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)",
		)
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
		ID:                          dbtest.DefaultWorkerGroupID,
		RegionID:                    dbtest.DefaultRegionID,
		Name:                        dbtest.DefaultWorkerGroupID,
		EnrollmentPolicyFingerprint: "sha256:test-worker-group",
		AllowsRun:                   true,
		AllowsBuild:                 true,
		RequiredCpuMillis:           1,
		RequiredMemoryBytes:         1,
		RequiredWorkloadDiskBytes:   1,
		RequiredScratchBytes:        1,
		RequiredVmSlots:             1,
		RequiredBuildExecutors:      1,
		ProtocolVersion:             api.CurrentWorkerProtocolVersion,
		AllowedAttestationFingerprints: []string{
			"sha256:test-attestation",
		},
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
