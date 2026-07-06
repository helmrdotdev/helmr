package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) validateWorkerWorkspacePrimitiveScope(ctx context.Context, worker workerActor, scope api.WorkerWorkspacePrimitiveScope) (db.WorkspaceMount, error) {
	orgID, err := parseRequiredWorkspaceUUID("org_id", scope.OrgID)
	if err != nil {
		return db.WorkspaceMount{}, badRequest(err)
	}
	projectID, err := parseRequiredWorkspaceUUID("project_id", scope.ProjectID)
	if err != nil {
		return db.WorkspaceMount{}, badRequest(err)
	}
	environmentID, err := parseRequiredWorkspaceUUID("environment_id", scope.EnvironmentID)
	if err != nil {
		return db.WorkspaceMount{}, badRequest(err)
	}
	workspaceID, err := parseRequiredWorkspaceUUID("workspace_id", scope.WorkspaceID)
	if err != nil {
		return db.WorkspaceMount{}, badRequest(err)
	}
	workspaceMountID, err := parseRequiredWorkspaceUUID("workspace_mount_id", scope.WorkspaceMountID)
	if err != nil {
		return db.WorkspaceMount{}, badRequest(err)
	}
	runtimeInstanceToken := strings.TrimSpace(scope.RuntimeInstanceToken)
	if runtimeInstanceToken == "" {
		return db.WorkspaceMount{}, badRequest(errors.New("runtime_instance_token is required"))
	}
	mount, err := s.db.GetWorkspaceMountForWorkerPrimitiveScope(ctx, db.GetWorkspaceMountForWorkerPrimitiveScopeParams{
		OrgID:                orgID,
		WorkerGroupID:        worker.WorkerGroupID,
		ProjectID:            projectID,
		EnvironmentID:        environmentID,
		WorkspaceID:          workspaceID,
		ID:                   workspaceMountID,
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		RuntimeInstanceToken: runtimeInstanceToken,
	})
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return mount, nil
}

func parseWorkerPrimitiveUUID(field string, raw string) (pgtype.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(id), nil
}

func workerPrimitiveWorkspaceMountMatches(actual pgtype.UUID, expected pgtype.UUID) bool {
	return actual.Valid && pgvalue.MustUUIDValue(actual) == pgvalue.MustUUIDValue(expected)
}

func workerPrimitiveInt4Equal(actual pgtype.Int4, expected pgtype.Int4) bool {
	if actual.Valid != expected.Valid {
		return false
	}
	return !actual.Valid || actual.Int32 == expected.Int32
}

func workerPrimitiveJSONEqual(actual []byte, expected []byte) bool {
	if len(bytes.TrimSpace(actual)) == 0 {
		actual = []byte(`{}`)
	}
	if len(bytes.TrimSpace(expected)) == 0 {
		expected = []byte(`{}`)
	}
	actualCanonical, err := canonicalJSON(json.RawMessage(actual))
	if err != nil {
		return false
	}
	expectedCanonical, err := canonicalJSON(json.RawMessage(expected))
	if err != nil {
		return false
	}
	return bytes.Equal(actualCanonical, expectedCanonical)
}
