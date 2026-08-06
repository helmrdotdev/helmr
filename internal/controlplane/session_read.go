package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	sessionListDefaultLimit = int32(50)
	sessionListMaxLimit     = int32(100)
)

type sessionListCursor struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	CreatedAt     string `json:"created_at"`
	SessionID     string `json:"session_id"`
}

type parsedSessionListQuery struct {
	limit          int32
	cursor         *sessionListCursor
	actorID        string
	key            string
	exactKeyLookup bool
}

type sessionProjectionRow struct {
	id           pgtype.UUID
	actorID      string
	deploymentID pgtype.UUID
	key          pgtype.Text
	state        string
	createdAt    pgtype.Timestamptz
	updatedAt    pgtype.Timestamptz
	currentRunID pgtype.UUID
	failure      []byte
	failureRunID pgtype.UUID
}

func (s *Server) listSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_query", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	query, err := parseSessionListQuery(r.URL.RawQuery, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_query", message: err.Error()}))
		return
	}
	response := api.ListSessionsResponse{Sessions: []api.Session{}}
	if query.exactKeyLookup {
		row, err := s.db.GetSessionSnapshotByKey(r.Context(), db.GetSessionSnapshotByKeyParams{
			OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
			ActorDeclaredID: query.actorID, Key: pgvalue.Text(query.key),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, response)
			return
		}
		if err != nil {
			s.writeSessionReadAuthorityError(w, err)
			return
		}
		item, err := projectSession(sessionProjectionFromKeyRow(row))
		if err != nil {
			s.writeSessionReadAuthorityError(w, err)
			return
		}
		response.Sessions = append(response.Sessions, item)
		writeJSON(w, http.StatusOK, response)
		return
	}
	var afterCreatedAt pgtype.Timestamptz
	var afterID pgtype.UUID
	if query.cursor != nil {
		createdAt, err := time.Parse(time.RFC3339Nano, query.cursor.CreatedAt)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_session_cursor", message: "session cursor is invalid"}))
			return
		}
		sessionID, err := ids.Parse(query.cursor.SessionID)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_session_cursor", message: "session cursor is invalid"}))
			return
		}
		afterCreatedAt = pgvalue.Timestamptz(createdAt)
		afterID = pgvalue.UUID(sessionID)
	}
	rows, err := s.db.ListSessionSnapshots(r.Context(), db.ListSessionSnapshotsParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		AfterCreatedAt: afterCreatedAt, AfterID: afterID, LimitCount: query.limit + 1,
	})
	if err != nil {
		s.writeSessionReadAuthorityError(w, err)
		return
	}
	hasMore := len(rows) > int(query.limit)
	if hasMore {
		rows = rows[:query.limit]
	}
	for _, row := range rows {
		item, err := projectSession(sessionProjectionFromListRow(row))
		if err != nil {
			s.writeSessionReadAuthorityError(w, err)
			return
		}
		response.Sessions = append(response.Sessions, item)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeSessionListCursor(sessionListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			SessionID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.writeSessionReadAuthorityError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSessionHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	row, err := s.db.GetSessionSnapshot(r.Context(), db.GetSessionSnapshotParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, ID: pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "session_not_found", message: "session was not found"}))
		return
	}
	if err != nil {
		s.writeSessionReadAuthorityError(w, err)
		return
	}
	item, err := projectSession(sessionProjectionFromGetRow(row))
	if err != nil {
		s.writeSessionReadAuthorityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func parseSessionListQuery(rawQuery, projectID, environmentID string) (parsedSessionListQuery, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return parsedSessionListQuery{}, errors.New("query string is malformed")
	}
	for name, entries := range values {
		switch name {
		case "cursor", "limit", "actor_id", "key":
		default:
			return parsedSessionListQuery{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return parsedSessionListQuery{}, fmt.Errorf("query parameter %q must appear once with a non-empty value", name)
		}
	}
	hasActorID := values.Has("actor_id")
	hasKey := values.Has("key")
	if hasActorID != hasKey {
		return parsedSessionListQuery{}, errors.New("actor_id and key must be provided together")
	}
	if hasActorID {
		if values.Has("cursor") || values.Has("limit") {
			return parsedSessionListQuery{}, errors.New("cursor and limit are not allowed with actor_id and key")
		}
		actorID := values.Get("actor_id")
		key := values.Get("key")
		if err := api.ValidateActorDeclaredID(actorID); err != nil {
			return parsedSessionListQuery{}, err
		}
		if err := api.ValidateActorKey(key); err != nil {
			return parsedSessionListQuery{}, err
		}
		return parsedSessionListQuery{actorID: actorID, key: key, exactKeyLookup: true}, nil
	}
	query := parsedSessionListQuery{limit: sessionListDefaultLimit}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || limit < 1 || limit > int64(sessionListMaxLimit) {
			return parsedSessionListQuery{}, fmt.Errorf("limit must be an integer in [1,%d]", sessionListMaxLimit)
		}
		query.limit = int32(limit)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeSessionListCursor(raw)
		if err != nil {
			return parsedSessionListQuery{}, err
		}
		if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
			return parsedSessionListQuery{}, errors.New("session cursor belongs to another scope")
		}
		if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
			return parsedSessionListQuery{}, errors.New("session cursor is invalid")
		}
		if err := ids.Validate(cursor.SessionID); err != nil {
			return parsedSessionListQuery{}, errors.New("session cursor is invalid")
		}
		query.cursor = &cursor
	}
	return query, nil
}

func encodeSessionListCursor(cursor sessionListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSessionListCursor(raw string) (sessionListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return sessionListCursor{}, errors.New("session cursor is invalid")
	}
	var cursor sessionListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.ProjectID == "" || cursor.EnvironmentID == "" || cursor.CreatedAt == "" || cursor.SessionID == "" {
		return sessionListCursor{}, errors.New("session cursor is invalid")
	}
	return cursor, nil
}

func projectSession(row sessionProjectionRow) (api.Session, error) {
	status, err := projectSessionStatus(sessionReadRecord{
		id: row.id, key: row.key, state: row.state, createdAt: row.createdAt,
		updatedAt: row.updatedAt, currentRunID: row.currentRunID,
		failure: row.failure, failureRunID: row.failureRunID,
	})
	if err != nil {
		return api.Session{}, err
	}
	deploymentID := pgvalue.UUIDString(row.deploymentID)
	if err := ids.Validate(deploymentID); err != nil {
		return api.Session{}, errors.New("session Deployment ID is invalid")
	}
	return api.Session{
		ID: status.id, ActorID: row.actorID, DeploymentID: deploymentID,
		Key: status.key, Status: status.status, CreatedAt: status.createdAt,
		UpdatedAt: status.updatedAt, CurrentRunID: status.currentRunID, Failure: status.failure,
	}, nil
}

func sessionProjectionFromGetRow(row db.GetSessionSnapshotRow) sessionProjectionRow {
	return sessionProjectionRow{id: row.ID, actorID: row.ActorDeclaredID, deploymentID: row.DeploymentID, key: row.Key, state: row.State, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, currentRunID: row.CurrentRunID, failure: row.Failure, failureRunID: row.FailureRunID}
}

func sessionProjectionFromKeyRow(row db.GetSessionSnapshotByKeyRow) sessionProjectionRow {
	return sessionProjectionRow{id: row.ID, actorID: row.ActorDeclaredID, deploymentID: row.DeploymentID, key: row.Key, state: row.State, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, currentRunID: row.CurrentRunID, failure: row.Failure, failureRunID: row.FailureRunID}
}

func sessionProjectionFromListRow(row db.ListSessionSnapshotsRow) sessionProjectionRow {
	return sessionProjectionRow{id: row.ID, actorID: row.ActorDeclaredID, deploymentID: row.DeploymentID, key: row.Key, state: row.State, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, currentRunID: row.CurrentRunID, failure: row.Failure, failureRunID: row.FailureRunID}
}

func (s *Server) writeSessionReadAuthorityError(w http.ResponseWriter, err error) {
	s.log.Error("read Session failed", "error", err)
	writeError(w, unavailable(codedError{code: "session_authority_unavailable", message: "Session authority is unavailable", retryable: true}))
}

func writeSessionReadAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Session read authentication failed", "error", err)
		writeError(w, unavailable(codedError{code: "session_authority_unavailable", message: "Session authentication is unavailable", retryable: true}))
		return
	}
	writeError(w, unauthorized(codedError{code: "authentication_required", message: "authentication is required"}))
}
