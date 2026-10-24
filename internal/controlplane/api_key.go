package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/ids"
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
		item.Permissions = apiKeyPermissionGrantsFromPermissions(row.Permissions)
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
	permissionGrants, permissions, err := normalizeAPIKeyPermissionGrants(input.Permissions)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	issued, err := identity.IssueAPIKey(r.Context(), s.tx, principal, scope, identity.APIKeyInput{
		Name:          input.Name,
		Permissions:   permissions,
		ExpiresInDays: input.ExpiresInDays,
	})
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(issued.Record, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, errors.New("format api key"))
		return
	}
	summary.Permissions = permissionGrants
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

// normalizeAPIKeyPermissionGrants maps requested permission grants to the
// sorted, deduplicated permissions they name and the single canonical grant
// that lists them.
func normalizeAPIKeyPermissionGrants(grants []api.APIKeyPermissionGrant) ([]api.APIKeyPermissionGrant, []auth.Permission, error) {
	if len(grants) == 0 {
		return nil, nil, errors.New("permissions must include at least one grant")
	}
	permissions := make([]auth.Permission, 0, len(grants))
	for _, grant := range grants {
		if len(grant.Scopes) == 0 {
			return nil, nil, errors.New("permission grants must include at least one scope")
		}
		for _, scope := range grant.Scopes {
			normalizedScope, ok := normalizeAPIKeyScope(scope)
			if !ok {
				return nil, nil, fmt.Errorf("unsupported permission scope %q", scope)
			}
			permission, ok := apiKeyScopePermission(normalizedScope)
			if !ok {
				return nil, nil, fmt.Errorf("unsupported permission scope %q", scope)
			}
			if !slices.Contains(permissions, permission) {
				permissions = append(permissions, permission)
			}
		}
	}
	slices.Sort(permissions)
	scopes := make([]api.APIKeyScope, 0, len(permissions))
	for _, permission := range permissions {
		scope, ok := apiKeyPermissionScope(string(permission))
		if !ok {
			return nil, nil, fmt.Errorf("unsupported permission %q", permission)
		}
		scopes = append(scopes, scope)
	}
	return []api.APIKeyPermissionGrant{{Scopes: scopes}}, permissions, nil
}

func normalizeAPIKeyScope(scope api.APIKeyScope) (api.APIKeyScope, bool) {
	value := strings.TrimSpace(string(scope))
	return api.APIKeyScope(value), auth.IsCapability(auth.Permission(value))
}
func apiKeyScopePermission(scope api.APIKeyScope) (auth.Permission, bool) {
	return auth.Permission(scope), auth.IsCapability(auth.Permission(scope))
}
func apiKeyPermissionScope(permission string) (api.APIKeyScope, bool) {
	return normalizeAPIKeyScope(api.APIKeyScope(permission))
}

func apiKeyPermissionGrantsFromPermissions(permissions []string) []api.APIKeyPermissionGrant {
	scopes := make([]api.APIKeyScope, 0, len(permissions))
	for _, permission := range permissions {
		scope, ok := apiKeyPermissionScope(permission)
		if !ok {
			continue
		}
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		return nil
	}
	return []api.APIKeyPermissionGrant{{Scopes: scopes}}
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
	_, permissions, err := normalizeAPIKeyPermissionGrants(input.Permissions)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := identity.UpdateAPIKey(r.Context(), s.tx, principal, scope, id, identity.APIKeyUpdate{Name: input.Name, Permissions: permissions, ExpiresAt: input.ExpiresAt})
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(record, chi.URLParam(r, "project"), chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, err)
		return
	}
	summary.Permissions = apiKeyPermissionGrantsFromPermissions(record.Permissions)
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
	summary.Permissions = apiKeyPermissionGrantsFromPermissions(result.Record.Permissions)
	writeJSON(w, http.StatusOK, api.APIKeyIssued{APIKeySummary: summary, RawKey: result.Raw})
}
