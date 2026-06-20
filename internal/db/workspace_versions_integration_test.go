package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestWorkspaceMaterializationRequiresReadyCurrentVersion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	versionID := uuid.Must(uuid.NewV7())
	digest := "sha256:" + strings.ReplaceAll(uuid.NewString(), "-", "")

	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes
		)
		VALUES ($1, $2, $3, $4, $5, 'system', 'capturing', $6, 'tar', 1, $7, 1)
	`, versionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, artifactID, digest); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND environment_id = $4
		   AND id = $5
	`, versionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	_, err = queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"test"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("EnsureWorkspaceMaterializationRequested err = %v, want no rows for non-ready current version", err)
	}
}

func TestWorkspaceCurrentVersionAllowsReadyVersionCreatedInSameTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
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
			id, org_id, project_id, environment_id, deployment_sandbox_id,
			sandbox_id, sandbox_fingerprint, current_version_id
		)
		VALUES ($1, $2, $3, $4, $5, 'default', 'sandbox-fingerprint', $6)
	`, workspaceID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		VALUES ($1, $2, $3, $4, $5, 'system', 'ready', $6, 'tar', 1, $7, 1, now())
	`, versionID, ids.orgID, ids.projectID, ids.environmentID, workspaceID, artifactID, digest); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ready current version: %v", err)
	}
}

func TestCreateWorkspaceFromSandboxCreatesInitialCurrentVersion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	workspaceID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	digest := "sha256:" + strings.ReplaceAll(uuid.NewString(), "-", "")

	created, err := queries.CreateWorkspaceFromSandbox(ctx, db.CreateWorkspaceFromSandboxParams{
		ID:                        pgvalue.UUID(workspaceID),
		OrgID:                     pgvalue.UUID(ids.orgID),
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ExternalID:                "created-from-sandbox",
		Metadata:                  []byte(`{"source":"test"}`),
		Tags:                      []string{"integration"},
		RetentionPolicy:           []byte(`{}`),
		InitialVersionID:          pgvalue.UUID(versionID),
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
