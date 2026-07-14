package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCloseWorkspaceExecStdinSetsClosedAt(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	process, err := queries.CreateWorkspaceExec(ctx, db.CreateWorkspaceExecParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Command:        []byte(`["cat"]`),
		EnvShape:       []byte(`{}`),
		FilesystemMode: db.WorkspaceFilesystemModeWrite,
		State:          db.WorkspaceProcessStateQueued,
		Detached:       false,
		OrgID:          pgvalue.UUID(ids.orgID),
		ProjectID:      pgvalue.UUID(ids.projectID),
		EnvironmentID:  pgvalue.UUID(ids.environmentID),
		WorkspaceID:    pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}

	closed, err := queries.CloseWorkspaceExecStdin(ctx, db.CloseWorkspaceExecStdinParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            process.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !closed.StdinClosedAt.Valid {
		t.Fatal("stdin_closed_at was not set")
	}
}
