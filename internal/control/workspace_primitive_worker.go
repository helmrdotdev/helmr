package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) validateWorkerWorkspacePrimitiveScope(ctx context.Context, worker workerActor, scope api.WorkerWorkspacePrimitiveScope) (db.WorkspaceMaterialization, error) {
	orgID, err := parseRequiredWorkspaceUUID("org_id", scope.OrgID)
	if err != nil {
		return db.WorkspaceMaterialization{}, badRequest(err)
	}
	projectID, err := parseRequiredWorkspaceUUID("project_id", scope.ProjectID)
	if err != nil {
		return db.WorkspaceMaterialization{}, badRequest(err)
	}
	environmentID, err := parseRequiredWorkspaceUUID("environment_id", scope.EnvironmentID)
	if err != nil {
		return db.WorkspaceMaterialization{}, badRequest(err)
	}
	workspaceID, err := parseRequiredWorkspaceUUID("workspace_id", scope.WorkspaceID)
	if err != nil {
		return db.WorkspaceMaterialization{}, badRequest(err)
	}
	materializationID, err := parseRequiredWorkspaceUUID("materialization_id", scope.MaterializationID)
	if err != nil {
		return db.WorkspaceMaterialization{}, badRequest(err)
	}
	reservationToken := strings.TrimSpace(scope.ReservationToken)
	if reservationToken == "" {
		return db.WorkspaceMaterialization{}, badRequest(errors.New("reservation_token is required"))
	}
	materialization, err := s.db.GetWorkspaceMaterialization(ctx, db.GetWorkspaceMaterializationParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
		ID:            materializationID,
	})
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	if !materialization.WorkerInstanceID.Valid || pgvalue.MustUUIDValue(materialization.WorkerInstanceID) != worker.WorkerInstanceID {
		return db.WorkspaceMaterialization{}, conflict(errors.New("workspace materialization is not reserved by this worker"))
	}
	if materialization.ReservationToken != reservationToken {
		return db.WorkspaceMaterialization{}, conflict(errors.New("workspace materialization reservation token is stale"))
	}
	if materialization.ReservationExpiresAt.Valid && time.Now().After(materialization.ReservationExpiresAt.Time) {
		return db.WorkspaceMaterialization{}, conflict(errors.New("workspace materialization reservation expired"))
	}
	switch materialization.State {
	case db.WorkspaceMaterializationStateRunning, db.WorkspaceMaterializationStateCapturing, db.WorkspaceMaterializationStateStopping:
	default:
		return db.WorkspaceMaterialization{}, conflict(errors.New("workspace materialization is not running"))
	}
	return materialization, nil
}

func parseWorkerPrimitiveUUID(field string, raw string) (pgtype.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(id), nil
}

func workerPrimitiveMaterializationMatches(actual pgtype.UUID, expected pgtype.UUID) bool {
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
