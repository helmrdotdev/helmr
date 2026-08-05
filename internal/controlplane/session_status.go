package controlplane

import (
	"context"
	"encoding/json"
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

type sessionReadRecord struct {
	id           pgtype.UUID
	key          pgtype.Text
	state        string
	createdAt    pgtype.Timestamptz
	updatedAt    pgtype.Timestamptz
	currentRunID pgtype.UUID
	failure      []byte
	failureRunID pgtype.UUID
}

func getSessionStatus(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	sessionID pgtype.UUID,
) (api.SessionStatusSnapshot, error) {
	row, err := store.GetSessionRead(ctx, db.GetSessionReadParams{
		EnvironmentID: environmentID, SessionID: sessionID,
	})
	if err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	return projectSessionStatus(sessionReadRecordFromGet(row))
}

func (s *Server) sessionReadScope(
	r *http.Request,
	principal auth.Actor,
) (auth.Scope, pgtype.UUID, error) {
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, err
	}
	scope, _, environmentID, err := s.requestEnvironmentScope(r.Context(), principal, projectRef, environmentRef)
	return scope, environmentID, err
}

func projectSessionStatus(record sessionReadRecord) (api.SessionStatusSnapshot, error) {
	id := pgvalue.UUIDString(record.id)
	if err := ids.Validate(id); err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	status, err := sessionStatus(record.state)
	if err != nil {
		return api.SessionStatusSnapshot{}, err
	}
	if !record.createdAt.Valid || !record.updatedAt.Valid {
		return api.SessionStatusSnapshot{}, errors.New("session timestamps are unavailable")
	}
	terminalFailure := status == api.SessionStatusFailed || status == api.SessionStatusCancelled
	if terminalFailure != (len(record.failure) > 0) {
		return api.SessionStatusSnapshot{}, errors.New("session failure projection is inconsistent")
	}
	if record.currentRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.currentRunID)); err != nil {
			return api.SessionStatusSnapshot{}, errors.New("session current run ID is invalid")
		}
	}
	if record.failureRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.failureRunID)); err != nil {
			return api.SessionStatusSnapshot{}, errors.New("session failure run ID is invalid")
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
	if terminalFailure {
		var failure api.SessionFailure
		if err := json.Unmarshal(record.failure, &failure); err != nil ||
			failure.Code == "" || failure.Message == "" {
			return api.SessionStatusSnapshot{}, errors.New("session failure is invalid")
		}
		if record.failureRunID.Valid && failure.Details.RunID != pgvalue.UUIDString(record.failureRunID) {
			return api.SessionStatusSnapshot{}, errors.New("session failure run is inconsistent")
		}
		result.Failure = &failure
	}
	return result, nil
}

func sessionStatus(state string) (api.SessionStatus, error) {
	switch state {
	case "open", "closing":
		return api.SessionStatusOpen, nil
	case "closed":
		return api.SessionStatusClosed, nil
	case "cancelled":
		return api.SessionStatusCancelled, nil
	case "failed":
		return api.SessionStatusFailed, nil
	default:
		return "", fmt.Errorf("session state %q has no public status", state)
	}
}

func sessionReadRecordFromGet(row db.Session) sessionReadRecord {
	return sessionReadRecord{
		id: row.ID, key: row.Key, state: row.State,
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
		currentRunID: row.CurrentRunID,
		failure:      row.Failure, failureRunID: row.FailureRunID,
	}
}
