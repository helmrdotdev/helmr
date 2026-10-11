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
		item, err := apiKeySummaryFromRow(row)
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
	issued, err := identity.IssueAPIKey(r.Context(), s.db, principal, scope, identity.APIKeyInput{
		Name:          input.Name,
		Permissions:   permissions,
		ExpiresInDays: input.ExpiresInDays,
	})
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	summary, err := apiKeySummaryFromRecord(issued.Record)
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
	if err := identity.RevokeAPIKey(r.Context(), s.db, principal, scope, id); err != nil {
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
	switch strings.TrimSpace(string(scope)) {
	case string(api.APIKeyScopeAsksRespond):
		return api.APIKeyScopeAsksRespond, true
	case string(api.APIKeyScopeSessionsRead):
		return api.APIKeyScopeSessionsRead, true
	case string(api.APIKeyScopeAgentsStart):
		return api.APIKeyScopeAgentsStart, true
	case string(api.APIKeyScopeSessionsSend):
		return api.APIKeyScopeSessionsSend, true
	case string(api.APIKeyScopeSessionsInterrupt):
		return api.APIKeyScopeSessionsInterrupt, true
	case string(api.APIKeyScopeSessionsResume):
		return api.APIKeyScopeSessionsResume, true
	case string(api.APIKeyScopeSessionsClose):
		return api.APIKeyScopeSessionsClose, true
	case string(api.APIKeyScopeSessionsCancel):
		return api.APIKeyScopeSessionsCancel, true
	case string(api.APIKeyScopeComputersCreate):
		return api.APIKeyScopeComputersCreate, true
	case string(api.APIKeyScopeComputersRead):
		return api.APIKeyScopeComputersRead, true
	case string(api.APIKeyScopeComputersDelete):
		return api.APIKeyScopeComputersDelete, true
	case string(api.APIKeyScopeComputerCommandCreate):
		return api.APIKeyScopeComputerCommandCreate, true
	case string(api.APIKeyScopeSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(api.APIKeyScopeDeploymentsWrite):
		return api.APIKeyScopeDeploymentsWrite, true
	default:
		return "", false
	}
}

func apiKeyScopePermission(scope api.APIKeyScope) (auth.Permission, bool) {
	switch scope {
	case api.APIKeyScopeAsksRespond:
		return auth.PermissionAsksRespond, true
	case api.APIKeyScopeSessionsRead:
		return auth.PermissionSessionsRead, true
	case api.APIKeyScopeAgentsStart:
		return auth.PermissionAgentsStart, true
	case api.APIKeyScopeSessionsSend:
		return auth.PermissionSessionsSend, true
	case api.APIKeyScopeSessionsInterrupt:
		return auth.PermissionSessionsInterrupt, true
	case api.APIKeyScopeSessionsResume:
		return auth.PermissionSessionsResume, true
	case api.APIKeyScopeSessionsClose:
		return auth.PermissionSessionsClose, true
	case api.APIKeyScopeSessionsCancel:
		return auth.PermissionSessionsCancel, true
	case api.APIKeyScopeComputersCreate:
		return auth.PermissionComputersCreate, true
	case api.APIKeyScopeComputersRead:
		return auth.PermissionComputersRead, true
	case api.APIKeyScopeComputersDelete:
		return auth.PermissionComputersDelete, true
	case api.APIKeyScopeComputerCommandCreate:
		return auth.PermissionComputerCommandCreate, true
	case api.APIKeyScopeSecretsWrite:
		return auth.PermissionSecretsWrite, true
	case api.APIKeyScopeDeploymentsWrite:
		return auth.PermissionDeploymentsWrite, true
	default:
		return "", false
	}
}

func apiKeyPermissionScope(permission string) (api.APIKeyScope, bool) {
	switch strings.TrimSpace(permission) {
	case string(auth.PermissionAsksRespond):
		return api.APIKeyScopeAsksRespond, true
	case string(auth.PermissionSessionsRead):
		return api.APIKeyScopeSessionsRead, true
	case string(auth.PermissionAgentsStart):
		return api.APIKeyScopeAgentsStart, true
	case string(auth.PermissionSessionsSend):
		return api.APIKeyScopeSessionsSend, true
	case string(auth.PermissionSessionsInterrupt):
		return api.APIKeyScopeSessionsInterrupt, true
	case string(auth.PermissionSessionsResume):
		return api.APIKeyScopeSessionsResume, true
	case string(auth.PermissionSessionsClose):
		return api.APIKeyScopeSessionsClose, true
	case string(auth.PermissionSessionsCancel):
		return api.APIKeyScopeSessionsCancel, true
	case string(auth.PermissionComputersCreate):
		return api.APIKeyScopeComputersCreate, true
	case string(auth.PermissionComputersRead):
		return api.APIKeyScopeComputersRead, true
	case string(auth.PermissionComputersDelete):
		return api.APIKeyScopeComputersDelete, true
	case string(auth.PermissionComputerCommandCreate):
		return api.APIKeyScopeComputerCommandCreate, true
	case string(auth.PermissionSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(auth.PermissionDeploymentsWrite):
		return api.APIKeyScopeDeploymentsWrite, true
	default:
		return "", false
	}
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

func apiKeySummaryFromRecord(record db.APIKey) (api.APIKeySummary, error) {
	return apiKeySummary(
		record.ID,
		record.Name,
		record.KeyPrefix,
		record.ProjectID,
		record.EnvironmentID,
		record.CreatedAt,
		record.LastUsedAt,
		record.ExpiresAt,
		record.RevokedAt,
	)
}

func apiKeySummaryFromRow(row db.ListAPIKeysRow) (api.APIKeySummary, error) {
	return apiKeySummary(
		row.ID,
		row.Name,
		row.KeyPrefix,
		row.ProjectID,
		row.EnvironmentID,
		row.CreatedAt,
		row.LastUsedAt,
		row.ExpiresAt,
		row.RevokedAt,
	)
}

func apiKeySummary(id pgtype.UUID, name string, keyPrefix string, projectID pgtype.UUID, environmentID pgtype.UUID, createdAt pgtype.Timestamptz, lastUsedAt pgtype.Timestamptz, expiresAt pgtype.Timestamptz, revokedAt pgtype.Timestamptz) (api.APIKeySummary, error) {
	parsedID, err := pgvalue.UUIDValue(id)
	if err != nil {
		return api.APIKeySummary{}, err
	}
	parsedProjectID, err := pgvalue.UUIDValue(projectID)
	if err != nil {
		return api.APIKeySummary{}, err
	}
	parsedEnvironmentID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		return api.APIKeySummary{}, err
	}
	status := api.APIKeyStatusActive
	if revokedAt.Valid {
		status = api.APIKeyStatusRevoked
	} else if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		status = api.APIKeyStatusExpired
	}
	return api.APIKeySummary{
		ID:            parsedID.String(),
		Name:          name,
		KeyPrefix:     keyPrefix,
		ProjectID:     parsedProjectID.String(),
		EnvironmentID: parsedEnvironmentID.String(),
		Status:        status,
		CreatedAt:     pgvalue.Time(createdAt),
		LastUsedAt:    pgvalue.TimePtr(lastUsedAt),
		ExpiresAt:     pgvalue.TimePtr(expiresAt),
		RevokedAt:     pgvalue.TimePtr(revokedAt),
	}, nil
}
