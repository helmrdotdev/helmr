package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const apiKeyListLimit = 200

type apiKeyListCursor struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Filter        string `json:"filter"`
	CreatedAt     string `json:"created_at"`
	ID            string `json:"id"`
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	filter, err := identity.ParseAPIKeyFilter(r.URL.Query().Get("filter"))
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var after *identity.APIKeyPosition
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		cursor, err := decodeAPIKeyListCursor(rawCursor)
		if err != nil || cursor.ProjectID != scope.ProjectID || cursor.EnvironmentID != scope.EnvironmentID || cursor.Filter != string(filter) {
			writeError(w, badRequest(errors.New("api key cursor is invalid")))
			return
		}
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			writeError(w, badRequest(errors.New("api key cursor is invalid")))
			return
		}
		after = &identity.APIKeyPosition{CreatedAt: createdAt, ID: uuid.MustParse(cursor.ID)}
	}
	rows, hasMore, err := identity.ListAPIKeys(r.Context(), s.db, principal, scope, filter, apiKeyListLimit, after)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	items := make([]api.APIKeySummary, 0, len(rows))
	for _, row := range rows {
		item, err := apiKeySummaryFromRow(row, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
		if err != nil {
			writeError(w, errors.New("format api key"))
			return
		}
		item.RoleNames = row.RoleNames
		items = append(items, item)
	}
	response := api.ListAPIKeysResponse{APIKeys: items}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeAPIKeyListCursor(apiKeyListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Filter: string(filter),
			CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list api keys"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func encodeAPIKeyListCursor(cursor apiKeyListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeAPIKeyListCursor(raw string) (apiKeyListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return apiKeyListCursor{}, errors.New("api key cursor is invalid")
	}
	var cursor apiKeyListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.ProjectID == "" || cursor.EnvironmentID == "" ||
		cursor.Filter == "" || cursor.CreatedAt == "" || ids.Validate(cursor.ID) != nil {
		return apiKeyListCursor{}, errors.New("api key cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	var input api.IssueAPIKeyRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, fmt.Errorf("invalid API key request JSON: %w", err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var role *identity.APIKeyRole
	if input.Role != nil {
		roleID, err := ids.Parse(input.Role.RoleID)
		if err != nil || roleID == uuid.Nil() {
			writeError(w, badRequest(errors.New("invalid role_id")))
			return
		}
		role = &identity.APIKeyRole{RoleID: roleID, ExpectedRoleRevision: input.Role.ExpectedRoleRevision, AgentsMode: input.Role.AgentsMode}
		for _, value := range input.Role.AgentIDs {
			id, err := ids.Parse(value)
			if err != nil || id == uuid.Nil() {
				writeError(w, badRequest(errors.New("invalid agent_id")))
				return
			}
			role.AgentIDs = append(role.AgentIDs, id)
		}
	}
	issued, err := identity.IssueAPIKey(r.Context(), s.tx, principal, scope, identity.APIKeyInput{
		Name:          input.Name,
		Role:          role,
		ExpiresInDays: input.ExpiresInDays,
	})
	if err != nil {
		writeError(w, apiKeyIssuanceError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(issued.Record, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, errors.New("format api key"))
		return
	}
	summary.RoleNames = issued.RoleNames
	writeJSON(w, http.StatusCreated, api.APIKeyIssued{APIKeySummary: summary, RawKey: issued.Raw})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, notFound(errors.New("api key not found")))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := identity.RevokeAPIKey(r.Context(), s.tx, principal, scope, id); err != nil {
		writeError(w, identityError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func apiKeySummaryFromRecord(record db.APIKey, project, environment string) (api.APIKeySummary, error) {
	return apiKeySummary(
		record.ID,
		record.CreatedByMemberID,
		record.Name,
		record.KeyPrefix,
		project,
		environment,
		record.CreatedAt,
		record.LastUsedAt,
		record.ExpiresAt,
		record.RevokedAt,
	)
}

func apiKeySummaryFromRow(row db.ListAPIKeysRow, project, environment string) (api.APIKeySummary, error) {
	return apiKeySummary(
		row.ID,
		row.CreatedByMemberID,
		row.Name,
		row.KeyPrefix,
		project,
		environment,
		row.CreatedAt,
		row.LastUsedAt,
		row.ExpiresAt,
		row.RevokedAt,
	)
}

func apiKeySummary(id pgtype.UUID, createdBy pgtype.UUID, name string, keyPrefix string, project string, environment string, createdAt pgtype.Timestamptz, lastUsedAt pgtype.Timestamptz, expiresAt pgtype.Timestamptz, revokedAt pgtype.Timestamptz) (api.APIKeySummary, error) {
	parsedID, err := pgvalue.UUIDValue(id)
	if err != nil {
		return api.APIKeySummary{}, err
	}
	var creator *string
	if createdBy.Valid {
		id, err := pgvalue.UUIDValue(createdBy)
		if err != nil {
			return api.APIKeySummary{}, err
		}
		value := id.String()
		creator = &value
	}
	status := api.APIKeyStatusActive
	if revokedAt.Valid {
		status = api.APIKeyStatusRevoked
	} else if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		status = api.APIKeyStatusExpired
	}
	return api.APIKeySummary{
		ID:                parsedID.String(),
		RoleNames:         []string{},
		CreatedByMemberID: creator,
		Name:              name,
		KeyPrefix:         keyPrefix,
		Project:           project,
		Environment:       environment,
		Status:            status,
		CreatedAt:         pgvalue.Time(createdAt),
		LastUsedAt:        pgvalue.TimePtr(lastUsedAt),
		ExpiresAt:         pgvalue.TimePtr(expiresAt),
		RevokedAt:         pgvalue.TimePtr(revokedAt),
	}, nil
}

func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, notFound(err))
		return
	}
	var input api.UpdateAPIKeyRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := identity.UpdateAPIKey(r.Context(), s.tx, principal, scope, id, identity.APIKeyUpdate{Name: input.Name, ExpiresAt: input.ExpiresAt})
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(record, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, err)
		return
	}
	summary.RoleNames, err = s.db.ListAPIKeyRoleNames(r.Context(), db.ListAPIKeyRoleNamesParams{OrgID: record.OrgID, ApiKeyID: record.ID})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
func (s *Server) rotateAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, notFound(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := identity.RotateAPIKey(r.Context(), s.tx, principal, scope, id)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(result.Record, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, err)
		return
	}
	summary.RoleNames = result.RoleNames
	writeJSON(w, http.StatusOK, api.APIKeyIssued{APIKeySummary: summary, RawKey: result.Raw})
}

// Role assignment failures retain the same status as the ordinary access API.
func apiKeyIssuanceError(err error) error {
	var inputError org.InputError
	if errors.Is(err, org.ErrMemberManagementRequired) || errors.Is(err, org.ErrAccessNotFound) || errors.Is(err, org.ErrRoleTarget) || errors.Is(err, org.ErrStaleRevision) || errors.As(err, &inputError) {
		return permissionManagementError(err)
	}
	return identityError(err)
}
