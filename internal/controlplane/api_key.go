package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
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
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "active"
	}
	if !validAPIKeyFilter(filter) {
		writeError(w, badRequest(errors.New("filter must be active, expired, revoked, or all")))
		return
	}
	actor := actorFromContext(r.Context())
	_, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var afterCreatedAt pgtype.Timestamptz
	var afterID pgtype.UUID
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		cursor, err := decodeAPIKeyListCursor(rawCursor)
		if err != nil || cursor.ProjectID != pgvalue.UUIDString(projectUUID) ||
			cursor.EnvironmentID != pgvalue.UUIDString(environmentUUID) || cursor.Filter != filter {
			writeError(w, badRequest(errors.New("api key cursor is invalid")))
			return
		}
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			writeError(w, badRequest(errors.New("api key cursor is invalid")))
			return
		}
		afterCreatedAt = pgvalue.Timestamptz(createdAt)
		afterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := s.db.ListAPIKeys(r.Context(), db.ListAPIKeysParams{
		OrgID:          pgvalue.UUID(actor.OrgID),
		ProjectID:      projectUUID,
		EnvironmentID:  environmentUUID,
		StatusFilter:   filter,
		AfterCreatedAt: afterCreatedAt,
		AfterID:        afterID,
		RowLimit:       apiKeyListLimit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list api keys"))
		return
	}
	hasMore := len(rows) > apiKeyListLimit
	if hasMore {
		rows = rows[:apiKeyListLimit]
	}
	items := make([]api.APIKeySummary, 0, len(rows))
	for _, row := range rows {
		item, err := apiKeySummaryFromRow(row)
		if err != nil {
			writeError(w, errors.New("format api key"))
			return
		}
		grants, err := s.db.ListAPIKeyGrants(r.Context(), db.ListAPIKeyGrantsParams{
			OrgID:    row.OrgID,
			APIKeyID: row.ID,
		})
		if err != nil {
			writeError(w, errors.New("list api key permissions"))
			return
		}
		item.Permissions = apiKeyPermissionGrantsFromRows(grants)
		items = append(items, item)
	}
	response := api.ListAPIKeysResponse{APIKeys: items}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeAPIKeyListCursor(apiKeyListCursor{
			ProjectID: pgvalue.UUIDString(projectUUID), EnvironmentID: pgvalue.UUIDString(environmentUUID), Filter: filter,
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
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid API key request JSON: %w", err)))
		return
	}
	name := strings.TrimSpace(input.Name)
	if !validAPIKeyName(name) {
		writeError(w, badRequest(errors.New("name must be 1-64 characters and contain no control characters")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	permissionGrants, err := normalizeAPIKeyPermissionGrants(input.Permissions)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	expiresAt := pgtype.Timestamptz{}
	if input.ExpiresInDays != nil {
		if !validAPIKeyExpiryDays(*input.ExpiresInDays) {
			writeError(w, badRequest(errors.New("expires_in_days must be 30, 90, or 365")))
			return
		}
		expiresAt = pgvalue.Timestamptz(time.Now().AddDate(0, 0, *input.ExpiresInDays))
	}
	generated, err := auth.GenerateAPIKey()
	if err != nil {
		writeError(w, errors.New("generate api key"))
		return
	}
	record, err := s.db.IssueAPIKey(r.Context(), db.IssueAPIKeyParams{
		ID:              pgvalue.UUID(uuid.NewV7()),
		OrgID:           pgvalue.UUID(actor.OrgID),
		ProjectID:       projectUUID,
		EnvironmentID:   environmentUUID,
		CreatedByUserID: pgvalue.UUID(actor.UserID),
		Role:            db.OrgMemberRole(actor.Role),
		Name:            name,
		KeyPrefix:       generated.KeyPrefix,
		TokenHash:       generated.TokenHash,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		writeError(w, errors.New("create api key"))
		return
	}
	for _, grant := range permissionGrants {
		for _, scope := range grant.Scopes {
			permission, ok := apiKeyScopePermission(scope)
			if !ok {
				writeError(w, badRequest(fmt.Errorf("unsupported permission scope %q", scope)))
				return
			}
			if _, err := s.db.CreateAPIKeyGrant(r.Context(), db.CreateAPIKeyGrantParams{
				ID:              pgvalue.UUID(uuid.NewV7()),
				OrgID:           pgvalue.UUID(actor.OrgID),
				APIKeyID:        record.ID,
				Permission:      string(permission),
				CreatedByUserID: pgvalue.UUID(actor.UserID),
			}); err != nil {
				writeError(w, errors.New("create api key permission"))
				return
			}
		}
	}
	summary, err := apiKeySummaryFromRecord(record)
	if err != nil {
		writeError(w, errors.New("format api key"))
		return
	}
	summary.ProjectID = scope.ProjectID
	summary.EnvironmentID = scope.EnvironmentID
	summary.Permissions = permissionGrants
	writeJSON(w, http.StatusCreated, api.APIKeyIssued{APIKeySummary: summary, RawKey: generated.Raw})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, notFound(errors.New("api key not found")))
		return
	}
	actor := actorFromContext(r.Context())
	_, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.RevokeAPIKey(r.Context(), db.RevokeAPIKeyParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectUUID,
		EnvironmentID: environmentUUID,
		ID:            pgvalue.UUID(id),
	})
	if err != nil {
		writeError(w, errors.New("revoke api key"))
		return
	}
	if rows == 0 {
		writeError(w, notFound(errors.New("api key not found")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validAPIKeyName(name string) bool {
	return name != "" && len(name) <= 64 && !strings.ContainsFunc(name, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}

func validAPIKeyExpiryDays(days int) bool {
	switch days {
	case 30, 90, 365:
		return true
	default:
		return false
	}
}

func validAPIKeyFilter(filter string) bool {
	switch filter {
	case "active", "expired", "revoked", "all":
		return true
	default:
		return false
	}
}

func normalizeAPIKeyPermissionGrants(grants []api.APIKeyPermissionGrant) ([]api.APIKeyPermissionGrant, error) {
	if len(grants) == 0 {
		return nil, errors.New("permissions must include at least one grant")
	}
	scopes := make([]api.APIKeyScope, 0, len(grants))
	seen := map[api.APIKeyScope]struct{}{}
	for _, grant := range grants {
		if len(grant.Scopes) == 0 {
			return nil, errors.New("permission grants must include at least one scope")
		}
		for _, scope := range grant.Scopes {
			normalizedScope, ok := normalizeAPIKeyScope(scope)
			if !ok {
				return nil, fmt.Errorf("unsupported permission scope %q", scope)
			}
			if _, ok := seen[normalizedScope]; ok {
				continue
			}
			seen[normalizedScope] = struct{}{}
			scopes = append(scopes, normalizedScope)
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("permissions must include at least one supported scope")
	}
	return []api.APIKeyPermissionGrant{{Scopes: scopes}}, nil
}

func normalizeAPIKeyScope(scope api.APIKeyScope) (api.APIKeyScope, bool) {
	switch strings.TrimSpace(string(scope)) {
	case string(api.APIKeyScopeRunsCreate):
		return api.APIKeyScopeRunsCreate, true
	case string(api.APIKeyScopeRunsRead):
		return api.APIKeyScopeRunsRead, true
	case string(api.APIKeyScopeRunsManage):
		return api.APIKeyScopeRunsManage, true
	case string(api.APIKeyScopeSessionsRead):
		return api.APIKeyScopeSessionsRead, true
	case string(api.APIKeyScopeActorsStart):
		return api.APIKeyScopeActorsStart, true
	case string(api.APIKeyScopeSessionsInputSend):
		return api.APIKeyScopeSessionsInputSend, true
	case string(api.APIKeyScopeSessionsClose):
		return api.APIKeyScopeSessionsClose, true
	case string(api.APIKeyScopeTokensCreate):
		return api.APIKeyScopeTokensCreate, true
	case string(api.APIKeyScopeTokensRead):
		return api.APIKeyScopeTokensRead, true
	case string(api.APIKeyScopeTokensComplete):
		return api.APIKeyScopeTokensComplete, true
	case string(api.APIKeyScopeTokensCancel):
		return api.APIKeyScopeTokensCancel, true
	case string(api.APIKeyScopeWorkspacesCreate):
		return api.APIKeyScopeWorkspacesCreate, true
	case string(api.APIKeyScopeWorkspacesRead):
		return api.APIKeyScopeWorkspacesRead, true
	case string(api.APIKeyScopeWorkspacesDelete):
		return api.APIKeyScopeWorkspacesDelete, true
	case string(api.APIKeyScopeWorkspaceFilesRead):
		return api.APIKeyScopeWorkspaceFilesRead, true
	case string(api.APIKeyScopeWorkspaceExecCreate):
		return api.APIKeyScopeWorkspaceExecCreate, true
	case string(api.APIKeyScopeSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(api.APIKeyScopeTasksDeploy):
		return api.APIKeyScopeTasksDeploy, true
	default:
		return "", false
	}
}

func apiKeyScopePermission(scope api.APIKeyScope) (auth.Permission, bool) {
	switch scope {
	case api.APIKeyScopeRunsCreate:
		return auth.PermissionRunsCreate, true
	case api.APIKeyScopeRunsRead:
		return auth.PermissionRunsRead, true
	case api.APIKeyScopeRunsManage:
		return auth.PermissionRunsManage, true
	case api.APIKeyScopeSessionsRead:
		return auth.PermissionSessionsRead, true
	case api.APIKeyScopeActorsStart:
		return auth.PermissionActorsStart, true
	case api.APIKeyScopeSessionsInputSend:
		return auth.PermissionSessionsInputSend, true
	case api.APIKeyScopeSessionsClose:
		return auth.PermissionSessionsClose, true
	case api.APIKeyScopeTokensCreate:
		return auth.PermissionTokensCreate, true
	case api.APIKeyScopeTokensRead:
		return auth.PermissionTokensRead, true
	case api.APIKeyScopeTokensComplete:
		return auth.PermissionTokensComplete, true
	case api.APIKeyScopeTokensCancel:
		return auth.PermissionTokensCancel, true
	case api.APIKeyScopeWorkspacesCreate:
		return auth.PermissionWorkspacesCreate, true
	case api.APIKeyScopeWorkspacesRead:
		return auth.PermissionWorkspacesRead, true
	case api.APIKeyScopeWorkspacesDelete:
		return auth.PermissionWorkspacesDelete, true
	case api.APIKeyScopeWorkspaceFilesRead:
		return auth.PermissionWorkspaceFilesRead, true
	case api.APIKeyScopeWorkspaceExecCreate:
		return auth.PermissionWorkspaceExecCreate, true
	case api.APIKeyScopeSecretsWrite:
		return auth.PermissionSecretsWrite, true
	case api.APIKeyScopeTasksDeploy:
		return auth.PermissionTasksDeploy, true
	default:
		return "", false
	}
}

func apiKeyPermissionScope(permission string) (api.APIKeyScope, bool) {
	switch strings.TrimSpace(permission) {
	case string(auth.PermissionRunsCreate):
		return api.APIKeyScopeRunsCreate, true
	case string(auth.PermissionRunsRead):
		return api.APIKeyScopeRunsRead, true
	case string(auth.PermissionRunsManage):
		return api.APIKeyScopeRunsManage, true
	case string(auth.PermissionSessionsRead):
		return api.APIKeyScopeSessionsRead, true
	case string(auth.PermissionActorsStart):
		return api.APIKeyScopeActorsStart, true
	case string(auth.PermissionSessionsInputSend):
		return api.APIKeyScopeSessionsInputSend, true
	case string(auth.PermissionSessionsClose):
		return api.APIKeyScopeSessionsClose, true
	case string(auth.PermissionTokensCreate):
		return api.APIKeyScopeTokensCreate, true
	case string(auth.PermissionTokensRead):
		return api.APIKeyScopeTokensRead, true
	case string(auth.PermissionTokensComplete):
		return api.APIKeyScopeTokensComplete, true
	case string(auth.PermissionTokensCancel):
		return api.APIKeyScopeTokensCancel, true
	case string(auth.PermissionWorkspacesCreate):
		return api.APIKeyScopeWorkspacesCreate, true
	case string(auth.PermissionWorkspacesRead):
		return api.APIKeyScopeWorkspacesRead, true
	case string(auth.PermissionWorkspacesDelete):
		return api.APIKeyScopeWorkspacesDelete, true
	case string(auth.PermissionWorkspaceFilesRead):
		return api.APIKeyScopeWorkspaceFilesRead, true
	case string(auth.PermissionWorkspaceExecCreate):
		return api.APIKeyScopeWorkspaceExecCreate, true
	case string(auth.PermissionSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(auth.PermissionTasksDeploy):
		return api.APIKeyScopeTasksDeploy, true
	default:
		return "", false
	}
}

func apiKeyPermissionGrantsFromRows(rows []db.APIKeyGrant) []api.APIKeyPermissionGrant {
	scopes := make([]api.APIKeyScope, 0, len(rows))
	for _, row := range rows {
		scope, ok := apiKeyPermissionScope(row.Permission)
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
