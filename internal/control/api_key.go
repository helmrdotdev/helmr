package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const apiKeyListLimit = 200

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
	_, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListAPIKeys(r.Context(), db.ListAPIKeysParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectUUID,
		EnvironmentID: environmentUUID,
		StatusFilter:  filter,
		RowLimit:      apiKeyListLimit + 1,
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
			ApiKeyID: row.ID,
		})
		if err != nil {
			writeError(w, errors.New("list api key permissions"))
			return
		}
		item.Permissions = apiKeyPermissionGrantsFromRows(grants)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, api.ListAPIKeysResponse{Items: items, HasMore: hasMore})
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
	scope, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
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
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
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
				ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:           pgvalue.UUID(actor.OrgID),
				ApiKeyID:        record.ID,
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, notFound(errors.New("api key not found")))
		return
	}
	actor := actorFromContext(r.Context())
	_, projectUUID, environmentUUID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
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
	case string(api.APIKeyScopeWorkspaceLifecycleManage):
		return api.APIKeyScopeWorkspaceLifecycleManage, true
	case string(api.APIKeyScopeWorkspaceFilesRead):
		return api.APIKeyScopeWorkspaceFilesRead, true
	case string(api.APIKeyScopeWorkspaceFilesWrite):
		return api.APIKeyScopeWorkspaceFilesWrite, true
	case string(api.APIKeyScopeWorkspaceVersionsRead):
		return api.APIKeyScopeWorkspaceVersionsRead, true
	case string(api.APIKeyScopeWorkspaceVersionsCapture):
		return api.APIKeyScopeWorkspaceVersionsCapture, true
	case string(api.APIKeyScopeWorkspaceVersionsRestore):
		return api.APIKeyScopeWorkspaceVersionsRestore, true
	case string(api.APIKeyScopeWorkspaceVersionsDiff):
		return api.APIKeyScopeWorkspaceVersionsDiff, true
	case string(api.APIKeyScopeWorkspaceExecCreate):
		return api.APIKeyScopeWorkspaceExecCreate, true
	case string(api.APIKeyScopeWorkspaceExecRead):
		return api.APIKeyScopeWorkspaceExecRead, true
	case string(api.APIKeyScopeWorkspaceExecManage):
		return api.APIKeyScopeWorkspaceExecManage, true
	case string(api.APIKeyScopeWorkspacePtyCreate):
		return api.APIKeyScopeWorkspacePtyCreate, true
	case string(api.APIKeyScopeWorkspacePtyRead):
		return api.APIKeyScopeWorkspacePtyRead, true
	case string(api.APIKeyScopeWorkspacePtyManage):
		return api.APIKeyScopeWorkspacePtyManage, true
	case string(api.APIKeyScopeWorkspacePortsExpose):
		return api.APIKeyScopeWorkspacePortsExpose, true
	case string(api.APIKeyScopeWorkspacePortsRead):
		return api.APIKeyScopeWorkspacePortsRead, true
	case string(api.APIKeyScopeWorkspacePortsClose):
		return api.APIKeyScopeWorkspacePortsClose, true
	case string(api.APIKeyScopeRunWaitpointsRead):
		return api.APIKeyScopeRunWaitpointsRead, true
	case string(api.APIKeyScopeChannelsWrite):
		return api.APIKeyScopeChannelsWrite, true
	case string(api.APIKeyScopeWaitpointTokensCreate):
		return api.APIKeyScopeWaitpointTokensCreate, true
	case string(api.APIKeyScopeWaitpointTokensRead):
		return api.APIKeyScopeWaitpointTokensRead, true
	case string(api.APIKeyScopeWaitpointTokensComplete):
		return api.APIKeyScopeWaitpointTokensComplete, true
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
	case api.APIKeyScopeWorkspaceLifecycleManage:
		return auth.PermissionWorkspaceLifecycleManage, true
	case api.APIKeyScopeWorkspaceFilesRead:
		return auth.PermissionFilesRead, true
	case api.APIKeyScopeWorkspaceFilesWrite:
		return auth.PermissionFilesWrite, true
	case api.APIKeyScopeWorkspaceVersionsRead:
		return auth.PermissionVersionsRead, true
	case api.APIKeyScopeWorkspaceVersionsCapture:
		return auth.PermissionVersionsCapture, true
	case api.APIKeyScopeWorkspaceVersionsRestore:
		return auth.PermissionVersionsRestore, true
	case api.APIKeyScopeWorkspaceVersionsDiff:
		return auth.PermissionVersionsDiff, true
	case api.APIKeyScopeWorkspaceExecCreate:
		return auth.PermissionExecCreate, true
	case api.APIKeyScopeWorkspaceExecRead:
		return auth.PermissionExecRead, true
	case api.APIKeyScopeWorkspaceExecManage:
		return auth.PermissionExecManage, true
	case api.APIKeyScopeWorkspacePtyCreate:
		return auth.PermissionPtyCreate, true
	case api.APIKeyScopeWorkspacePtyRead:
		return auth.PermissionPtyRead, true
	case api.APIKeyScopeWorkspacePtyManage:
		return auth.PermissionPtyManage, true
	case api.APIKeyScopeWorkspacePortsExpose:
		return auth.PermissionPortsExpose, true
	case api.APIKeyScopeWorkspacePortsRead:
		return auth.PermissionPortsRead, true
	case api.APIKeyScopeWorkspacePortsClose:
		return auth.PermissionPortsClose, true
	case api.APIKeyScopeRunWaitpointsRead:
		return auth.PermissionRunWaitpointsRead, true
	case api.APIKeyScopeChannelsWrite:
		return auth.PermissionChannelsWrite, true
	case api.APIKeyScopeWaitpointTokensCreate:
		return auth.PermissionWaitpointTokensCreate, true
	case api.APIKeyScopeWaitpointTokensRead:
		return auth.PermissionWaitpointTokensRead, true
	case api.APIKeyScopeWaitpointTokensComplete:
		return auth.PermissionWaitpointTokensComplete, true
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
	case string(auth.PermissionWorkspaceLifecycleManage):
		return api.APIKeyScopeWorkspaceLifecycleManage, true
	case string(auth.PermissionFilesRead):
		return api.APIKeyScopeWorkspaceFilesRead, true
	case string(auth.PermissionFilesWrite):
		return api.APIKeyScopeWorkspaceFilesWrite, true
	case string(auth.PermissionVersionsRead):
		return api.APIKeyScopeWorkspaceVersionsRead, true
	case string(auth.PermissionVersionsCapture):
		return api.APIKeyScopeWorkspaceVersionsCapture, true
	case string(auth.PermissionVersionsRestore):
		return api.APIKeyScopeWorkspaceVersionsRestore, true
	case string(auth.PermissionVersionsDiff):
		return api.APIKeyScopeWorkspaceVersionsDiff, true
	case string(auth.PermissionExecCreate):
		return api.APIKeyScopeWorkspaceExecCreate, true
	case string(auth.PermissionExecRead):
		return api.APIKeyScopeWorkspaceExecRead, true
	case string(auth.PermissionExecManage):
		return api.APIKeyScopeWorkspaceExecManage, true
	case string(auth.PermissionPtyCreate):
		return api.APIKeyScopeWorkspacePtyCreate, true
	case string(auth.PermissionPtyRead):
		return api.APIKeyScopeWorkspacePtyRead, true
	case string(auth.PermissionPtyManage):
		return api.APIKeyScopeWorkspacePtyManage, true
	case string(auth.PermissionPortsExpose):
		return api.APIKeyScopeWorkspacePortsExpose, true
	case string(auth.PermissionPortsRead):
		return api.APIKeyScopeWorkspacePortsRead, true
	case string(auth.PermissionPortsClose):
		return api.APIKeyScopeWorkspacePortsClose, true
	case string(auth.PermissionRunWaitpointsRead):
		return api.APIKeyScopeRunWaitpointsRead, true
	case string(auth.PermissionChannelsWrite):
		return api.APIKeyScopeChannelsWrite, true
	case string(auth.PermissionWaitpointTokensCreate):
		return api.APIKeyScopeWaitpointTokensCreate, true
	case string(auth.PermissionWaitpointTokensRead):
		return api.APIKeyScopeWaitpointTokensRead, true
	case string(auth.PermissionWaitpointTokensComplete):
		return api.APIKeyScopeWaitpointTokensComplete, true
	case string(auth.PermissionSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(auth.PermissionTasksDeploy):
		return api.APIKeyScopeTasksDeploy, true
	default:
		return "", false
	}
}

func apiKeyPermissionGrantsFromRows(rows []db.ApiKeyGrant) []api.APIKeyPermissionGrant {
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
