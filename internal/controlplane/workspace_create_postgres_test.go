package controlplane

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestRunPinnedWorkspaceCreateUsesSourceDeploymentAndFencesBeforeClaim(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceIDs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image",
		PayloadPresent: true,
		Payload:        []byte(`{"source":"workspace-create"}`),
		WorkspaceID:    sourceWorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	stale := errors.New("stale source run")
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
		DeclaredID: "workspace.v1", Key: &key,
		Secrets: []api.WorkspaceSecret{
			{Name: "API_TOKEN", Env: "API_TOKEN"},
		},
		IdempotencyKey: "create",
		Authorize: func(context.Context, db.Querier) error {
			return nil
		},
	}
	created, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Snapshot.ID != created.WorkspaceID.String() ||
		created.Snapshot.Status != api.WorkspaceStatusAvailable ||
		len(created.Snapshot.Secrets) != 1 ||
		created.Snapshot.Secrets[0] != (api.WorkspaceSecret{Name: "API_TOKEN", Env: "API_TOKEN"}) {
		t.Fatalf("creation snapshot = %+v", created.Snapshot)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces
		   SET state = 'deleting', desired_state = 'deleted', updated_at = now() + interval '1 minute'
		 WHERE id = $1
	`, created.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.WorkspaceID != created.WorkspaceID ||
		!reflect.DeepEqual(replayed.Snapshot, created.Snapshot) {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, fixture.deploymentID, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.server.createWorkspace(t.Context(), workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{Kind: workspaceDeclarationPromoted},
		DeclaredID:  request.DeclaredID, Key: request.Key, Secrets: request.Secrets,
		IdempotencyKey: request.IdempotencyKey,
	})
	var keyConflict WorkspaceKeyConflictError
	if !errors.As(err, &keyConflict) {
		t.Fatalf("cross-authority create error = %v, want WorkspaceKeyConflictError", err)
	}

	var deploymentID uuid.UUID
	var versionCount, secretPlacementCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT deployment_definitions.deployment_id,
		       (SELECT count(*) FROM workspace_versions WHERE workspace_id = workspaces.id),
		       (SELECT count(*) FROM workspace_secrets WHERE workspace_id = workspaces.id)
		  FROM workspaces
		  JOIN deployment_definitions
		    ON deployment_definitions.id = workspaces.deployment_definition_id
		 WHERE workspaces.id = $1
	`, created.WorkspaceID).Scan(&deploymentID, &versionCount, &secretPlacementCount); err != nil {
		t.Fatal(err)
	}
	if deploymentID != fixture.deploymentID || versionCount != 1 || secretPlacementCount != 1 {
		t.Fatalf(
			"deployment=%s versions=%d secret placements=%d",
			deploymentID,
			versionCount,
			secretPlacementCount,
		)
	}
}

func TestRunSourcedWorkspaceSelfExecAndDeleteAreBusyWithoutSideEffects(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceIDs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:     []byte(`{"source":"self-workspace"}`),
		WorkspaceID: sourceWorkspaceID,
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
