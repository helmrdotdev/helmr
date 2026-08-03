package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestRunPinnedWorkspaceCreateUsesSourceDeploymentAndFencesBeforeClaim(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceRefs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image",
		PayloadPresent: true,
		Payload:        []byte(`{"source":"workspace-create"}`),
		Workspace:      api.WorkspaceTarget{ID: &sourceWorkspaceID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	stale := errors.New("stale source Run")
	var claimsBefore int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.createWorkspace(t.Context(), workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{
			Kind: workspaceDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "workspace.v1", IdempotencyKey: "blocked",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	}); !errors.Is(err, stale) {
		t.Fatalf("stale source error = %v", err)
	}
	var claimsAfter int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsAfter); err != nil {
		t.Fatal(err)
	}
	if claimsAfter != claimsBefore {
		t.Fatalf("stale source changed claim count from %d to %d", claimsBefore, claimsAfter)
	}

	key := "run-pinned"
	request := workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{
			Kind: workspaceDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "workspace.v1", Key: &key, IdempotencyKey: "create",
		Authorize: func(context.Context, db.Querier) error {
			return nil
		},
	}
	created, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.WorkspaceID != created.WorkspaceID {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}

	var deploymentID uuid.UUID
	var versionCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT deployment_definitions.deployment_id,
		       (SELECT count(*) FROM workspace_versions WHERE workspace_id = workspaces.id)
		  FROM workspaces
		  JOIN deployment_definitions
		    ON deployment_definitions.id = workspaces.deployment_definition_id
		 WHERE workspaces.id = $1
	`, created.WorkspaceID).Scan(&deploymentID, &versionCount); err != nil {
		t.Fatal(err)
	}
	if deploymentID != fixture.deploymentID || versionCount != 1 {
		t.Fatalf("deployment=%s versions=%d", deploymentID, versionCount)
	}
}

func TestRunSourcedWorkspaceSelfExecAndDeleteAreBusyWithoutSideEffects(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceIDs[0]
	sourceWorkspaceRef := sourceWorkspaceID.String()
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:   []byte(`{"source":"self-workspace"}`),
		Workspace: api.WorkspaceTarget{ID: &sourceWorkspaceRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.server.db.GetWorkspace(
		t.Context(),
		db.GetWorkspaceParams{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), ID: pgvalue.UUID(sourceWorkspaceID),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var claimsBefore int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsBefore); err != nil {
		t.Fatal(err)
	}
	authorize := func(context.Context, db.Querier) error { return nil }
	stale := errors.New("stale source authority")
	_, err = fixture.server.admitWorkspaceExec(t.Context(), workspaceExecRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Workspace: record,
		Creator: workspaceExecCreator{
			SubjectType: "run", SubjectID: source.RunID.String(),
		},
		Command: []string{"true"}, IdempotencyKey: "stale-exec",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	})
	if !errors.Is(err, stale) {
		t.Fatalf("stale exec error = %v", err)
	}
	_, err = fixture.server.deleteWorkspace(t.Context(), workspaceDeleteRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		WorkspaceID: sourceWorkspaceID, IdempotencyKey: "stale-delete",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	})
	if !errors.Is(err, stale) {
		t.Fatalf("stale delete error = %v", err)
	}
	_, err = fixture.server.admitWorkspaceExec(t.Context(), workspaceExecRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Workspace: record,
		Creator: workspaceExecCreator{
			SubjectType: "run", SubjectID: source.RunID.String(),
		},
		Command: []string{"true"}, IdempotencyKey: "self-exec", Authorize: authorize,
	})
	if !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("self exec error = %v", err)
	}
	_, err = fixture.server.deleteWorkspace(t.Context(), workspaceDeleteRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		WorkspaceID: sourceWorkspaceID, IdempotencyKey: "self-delete", Authorize: authorize,
	})
	if !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("self delete error = %v", err)
	}
	var processCount, claimCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM workspace_processes WHERE workspace_id = $1
	`, pgvalue.MustUUIDValue(record.ID)).Scan(&processCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	var state db.WorkspaceState
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state FROM workspaces WHERE id = $1
	`, pgvalue.MustUUIDValue(record.ID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if processCount != 0 || claimCount != claimsBefore || state != db.WorkspaceStateActive {
		t.Fatalf("processes=%d claims=%d before=%d state=%s", processCount, claimCount, claimsBefore, state)
	}
}
