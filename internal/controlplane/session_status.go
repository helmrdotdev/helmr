package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type actorReadRecord struct {
	id           pgtype.UUID
	key          pgtype.Text
	state        string
	createdAt    pgtype.Timestamptz
	updatedAt    pgtype.Timestamptz
	currentRunID pgtype.UUID
	failureCode  pgtype.Text
	failureRunID pgtype.UUID
}

func getActorStatus(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	sessionID pgtype.UUID,
) (api.SessionStatusSnapshot, error) {
	row, err := store.GetActorRead(ctx, db.GetActorReadParams{
		EnvironmentID: environmentID, SessionID: sessionID,
	})
	if err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	return projectSessionStatus(actorReadRecordFromGet(row))
}

func (s *Server) actorReadScope(
	r *http.Request,
	principal auth.Actor,
) (auth.Scope, pgtype.UUID, error) {
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, err
	}
	scope, _, environmentID, err := s.requestEnvironmentScope(r.Context(), principal, projectRef, environmentRef)
	return scope, environmentID, err
}

func projectSessionStatus(record actorReadRecord) (api.SessionStatusSnapshot, error) {
	id := pgvalue.UUIDString(record.id)
	if err := ids.Validate(id); err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	status, err := sessionStatus(record.state)
	if err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	if !record.createdAt.Valid || !record.updatedAt.Valid {
		return api.SessionStatusSnapshot{}, errors.New("actor timestamps are unavailable")
	}
	failed := status == api.SessionStatusFailed
	if failed != (record.failureCode.Valid && record.failureRunID.Valid) {
		return api.SessionStatusSnapshot{}, errors.New("actor failure projection is inconsistent")
	}
	if !failed && (record.failureCode.Valid || record.failureRunID.Valid) {
		return api.SessionStatusSnapshot{}, errors.New("actor failure projection is inconsistent")
	}
	if record.currentRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.currentRunID)); err != nil {
			return api.SessionStatusSnapshot{}, errors.New("actor current run ID is invalid")
		}
	}
	if record.failureRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.failureRunID)); err != nil {
			return api.SessionStatusSnapshot{}, errors.New("actor failure run ID is invalid")
		}
	}
	if record.failureCode.Valid {
		switch record.failureCode.String {
		case "no_progress", "run_failed", "run_expired", "platform_failure":
		default:
			return api.SessionStatusSnapshot{}, errors.New("actor failure code is invalid")
		}
	}
	result := api.SessionStatusSnapshot{
		ID:        id,
		Status:    status,
		CreatedAt: record.createdAt.Time.UTC(),
		UpdatedAt: record.updatedAt.Time.UTC(),
	}
	if record.key.Valid {
		result.Key = &record.key.String
	}
	if record.currentRunID.Valid {
		value := pgvalue.UUIDString(record.currentRunID)
		result.CurrentRunID = &value
	}
	if failed {
		result.Failure = &api.SessionFailure{
			Code: record.failureCode.String, RunID: pgvalue.UUIDString(record.failureRunID),
		}
	}
	return result, nil
}

func sessionStatus(state string) (api.SessionStatus, error) {
	switch state {
	case "open", "closing":
		return api.SessionStatusOpen, nil
	case "closed":
		return api.SessionStatusClosed, nil
	case "cancelling", "cancelled":
		return api.SessionStatusCancelled, nil
	case "failed":
		return api.SessionStatusFailed, nil
	default:
		return "", fmt.Errorf("actor state %q has no public status", state)
	}
}

func actorReadRecordFromGet(row db.Session) actorReadRecord {
	return actorReadRecord{
		id: row.ID, key: row.Key, state: row.State,
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
		currentRunID: row.CurrentRunID,
		failureCode:  row.FailureCode, failureRunID: row.FailureRunID,
	}
}
