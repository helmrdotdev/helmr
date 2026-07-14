package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
)

func TestWorkspaceCurrentVersionAllowsReadyVersionCreatedInSameTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	workspaceID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	digest := "sha256:" + strings.ReplaceAll(uuid.NewString(), "-", "")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspaces (
			id, public_id, org_id, region_id, project_id, environment_id, deployment_sandbox_id,
			sandbox_id, sandbox_fingerprint, current_version_id
		)
		VALUES ($1, $8, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint', $7)
	`, workspaceID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, versionID, testWorkspacePublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		VALUES ($1, $8, $2, $3, $4, $5, 'system', 'ready', $6, 'tar', 1, $7, 1, now())
	`, versionID, ids.orgID, ids.projectID, ids.environmentID, workspaceID, artifactID, digest, testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ready current version: %v", err)
	}
}

func TestCreateWorkspaceFromSandboxCreatesInitialCurrentVersion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	workspaceID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	digest := "sha256:" + strings.ReplaceAll(uuid.NewString(), "-", "")

	created, err := queries.CreateWorkspaceFromSandbox(ctx, db.CreateWorkspaceFromSandboxParams{
		ID:                        pgvalue.UUID(workspaceID),
		PublicID:                  testWorkspacePublicID(t),
		OrgID:                     pgvalue.UUID(ids.orgID),
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ExternalID:                "created-from-sandbox",
		Metadata:                  []byte(`{"source":"test"}`),
		Tags:                      []string{"integration"},
		RetentionPolicy:           []byte(`{}`),
		InitialVersionID:          pgvalue.UUID(versionID),
		InitialVersionPublicID:    testWorkspaceVersionPublicID(t),
		InitialArtifactID:         pgvalue.UUID(artifactID),
		InitialArtifactEncoding:   "tar+gzip",
		InitialArtifactEntryCount: 1,
		InitialContentDigest:      digest,
		InitialSizeBytes:          10,
	})
	if err != nil {
		t.Fatalf("CreateWorkspaceFromSandbox: %v", err)
	}
	if got := pgvalue.MustUUIDValue(created.CurrentVersionID); got != versionID {
		t.Fatalf("current_version_id = %s, want %s", got, versionID)
	}

	var versionWorkspaceID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT workspace_id
		  FROM workspace_versions
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, versionID).Scan(&versionWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if versionWorkspaceID != workspaceID {
		t.Fatalf("version workspace_id = %s, want %s", versionWorkspaceID, workspaceID)
	}
}

func TestWorkspaceVersionReadQueriesRequireReadySameWorkspace(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	readyVersionID := uuid.Must(uuid.NewV7())
	newerReadyVersionID := uuid.Must(uuid.NewV7())
	systemReadyVersionID := uuid.Must(uuid.NewV7())
	capturingVersionID := uuid.Must(uuid.NewV7())
	otherWorkspaceID := uuid.Must(uuid.NewV7())
	otherVersionID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at, created_at
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'user', 'ready', $6, $7, 1, $8, 10, now(), now() - interval '1 minute')
	`, readyVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, artifactID, workspace.ArtifactEncoding, testDigest("ready-version"), testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at, created_at
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'user', 'ready', $6, $7, 1, $8, 10, now(), now())
	`, newerReadyVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, artifactID, workspace.ArtifactEncoding, testDigest("newer-ready-version"), testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at, created_at
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'system', 'ready', $6, $7, 1, $8, 10, now(), now() + interval '1 minute')
	`, systemReadyVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, artifactID, workspace.ArtifactEncoding, testDigest("system-ready-version"), testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'user', 'capturing', $6, $7, 1, $8, 10)
	`, capturingVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, artifactID, workspace.ArtifactEncoding, testDigest("capturing-version"), testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspaces (
			id, public_id, org_id, region_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint
		)
		VALUES ($1, $7, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint')
	`, otherWorkspaceID, ids.orgID, dbtest.DefaultRegionID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, testWorkspacePublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		VALUES ($1, $9, $2, $3, $4, $5, 'user', 'ready', $6, $7, 1, $8, 10, now())
	`, otherVersionID, ids.orgID, ids.projectID, ids.environmentID, otherWorkspaceID, artifactID, workspace.ArtifactEncoding, testDigest("other-workspace-version"), testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}

	got, err := queries.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(readyVersionID),
	})
	if err != nil {
		t.Fatalf("GetWorkspaceVersion ready: %v", err)
	}
	if pgvalue.MustUUIDValue(got.ID) != readyVersionID {
		t.Fatalf("version id = %s, want %s", pgvalue.MustUUIDValue(got.ID), readyVersionID)
	}

	_, err = queries.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(capturingVersionID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorkspaceVersion non-ready err = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(otherVersionID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetWorkspaceVersion cross-workspace err = %v, want pgx.ErrNoRows", err)
	}

	rows, err := queries.ListWorkspaceVersions(ctx, db.ListWorkspaceVersionsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Kind:          db.NullWorkspaceVersionKind{WorkspaceVersionKind: db.WorkspaceVersionKindUser, Valid: true},
		LimitCount:    10,
	})
	if err != nil {
		t.Fatalf("ListWorkspaceVersions: %v", err)
	}
	if len(rows) != 2 ||
		pgvalue.MustUUIDValue(rows[0].ID) != newerReadyVersionID ||
		pgvalue.MustUUIDValue(rows[1].ID) != readyVersionID {
		t.Fatalf("listed versions = %+v, want newest user versions %s then %s", rows, newerReadyVersionID, readyVersionID)
	}
}
