package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type sessionReadRecord struct {
	id           pgtype.UUID
	key          pgtype.Text
	status       string
	createdAt    pgtype.Timestamptz
	updatedAt    pgtype.Timestamptz
	currentRunID pgtype.UUID
	failure      []byte
	failureRunID pgtype.UUID
}

type sessionStatusProjection struct {
	id           string
	key          *string
	status       api.SessionStatus
	createdAt    time.Time
	updatedAt    time.Time
	currentRunID *string
	failure      *api.SessionFailure
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

func projectSessionStatus(record sessionReadRecord) (sessionStatusProjection, error) {
	id := pgvalue.UUIDString(record.id)
	if err := ids.Validate(id); err != nil {
		return sessionStatusProjection{}, err
	}
	status, err := sessionStatus(record.status)
	if err != nil {
		return sessionStatusProjection{}, err
	}
	if !record.createdAt.Valid || !record.updatedAt.Valid {
		return sessionStatusProjection{}, errors.New("session timestamps are unavailable")
	}
	terminalFailure := status == api.SessionStatusFailed
	if terminalFailure != (len(record.failure) > 0) {
		return sessionStatusProjection{}, errors.New("session failure projection is inconsistent")
	}
	if record.currentRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.currentRunID)); err != nil {
			return sessionStatusProjection{}, errors.New("session current run ID is invalid")
		}
	}
	if record.failureRunID.Valid {
		if err := ids.Validate(pgvalue.UUIDString(record.failureRunID)); err != nil {
			return sessionStatusProjection{}, errors.New("session failure run ID is invalid")
		}
	}
	result := sessionStatusProjection{
		id:        id,
		status:    status,
		createdAt: record.createdAt.Time.UTC(),
		updatedAt: record.updatedAt.Time.UTC(),
	}
	if record.key.Valid {
		result.key = &record.key.String
	}
	if record.currentRunID.Valid {
		value := pgvalue.UUIDString(record.currentRunID)
		result.currentRunID = &value
	}
	if terminalFailure {
		var failure api.SessionFailure
		if err := json.Unmarshal(record.failure, &failure); err != nil ||
			failure.Code == "" || failure.Message == "" {
			return sessionStatusProjection{}, errors.New("session failure is invalid")
		}
		if record.failureRunID.Valid && failure.Details.RunID != pgvalue.UUIDString(record.failureRunID) {
			return sessionStatusProjection{}, errors.New("session failure run is inconsistent")
		}
		result.failure = &failure
	}
	return result, nil
}

// sessionStorageStatuses maps public statuses directly to their stored values.
func sessionStorageStatuses(statuses []api.SessionStatus) []string {
	storedStatuses := make([]string, 0, len(statuses))
	for _, status := range statuses {
		storedStatuses = append(storedStatuses, string(status))
	}
	return storedStatuses
}

func sessionStatus(status string) (api.SessionStatus, error) {
	switch status {
	case "open":
		return api.SessionStatusOpen, nil
	case "closed":
		return api.SessionStatusClosed, nil
	case "closing":
		return api.SessionStatusClosing, nil
	case "failed":
		return api.SessionStatusFailed, nil
	default:
		return "", fmt.Errorf("session status %q has no public status", status)
	}
}
