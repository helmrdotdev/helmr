package control

import (
	"context"
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
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

const apiKeyListLimit = 200

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "active"
	}
	if !validAPIKeyFilter(filter) {
		writeError(w, http.StatusBadRequest, errors.New("filter must be active, expired, revoked, or all"))
		return
	}
	actor := actorFromContext(r.Context())
	rows, err := s.db.ListAPIKeys(r.Context(), db.ListAPIKeysParams{
		OrgID:        ids.ToPG(actor.OrgID),
		StatusFilter: filter,
		RowLimit:     apiKeyListLimit + 1,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list api keys"))
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
			writeError(w, http.StatusInternalServerError, errors.New("format api key"))
			return
		}
		grants, err := s.db.ListAPIKeyGrants(r.Context(), db.ListAPIKeyGrantsParams{
			OrgID:    row.OrgID,
			ApiKeyID: row.ID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("list api key permissions"))
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
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid API key request JSON: %w", err))
		return
	}
	name := strings.TrimSpace(input.Name)
	if !validAPIKeyName(name) {
		writeError(w, http.StatusBadRequest, errors.New("name must be 1-64 characters and contain no control characters"))
		return
	}
	permissionGrants, err := s.validateAPIKeyPermissionGrants(r.Context(), actorFromContext(r.Context()).OrgID, input.Permissions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	expiresAt := pgtype.Timestamptz{}
	if input.ExpiresInDays != nil {
		if !validAPIKeyExpiryDays(*input.ExpiresInDays) {
			writeError(w, http.StatusBadRequest, errors.New("expires_in_days must be 30, 90, or 365"))
			return
		}
		expiresAt = pgTimeToPG(time.Now().AddDate(0, 0, *input.ExpiresInDays))
	}
	generated, err := auth.GenerateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate api key"))
		return
	}
	actor := actorFromContext(r.Context())
	record, err := s.db.IssueAPIKey(r.Context(), db.IssueAPIKeyParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           ids.ToPG(actor.OrgID),
		CreatedByUserID: ids.ToPG(actor.UserID),
		Role:            db.OrgMemberRole(actor.Role),
		Name:            name,
		KeyPrefix:       generated.KeyPrefix,
		TokenHash:       generated.TokenHash,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create api key"))
		return
	}
	for _, grant := range permissionGrants {
		for _, scope := range grant.display.Scopes {
			permission, ok := apiKeyScopePermission(scope)
			if !ok {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported permission scope %q", scope))
				return
			}
			if _, err := s.db.CreateAPIKeyGrant(r.Context(), db.CreateAPIKeyGrantParams{
				ID:              ids.ToPG(ids.New()),
				OrgID:           ids.ToPG(actor.OrgID),
				ApiKeyID:        record.ID,
				ProjectID:       grant.projectID,
				EnvironmentID:   grant.environmentID,
				Permission:      string(permission),
				CreatedByUserID: ids.ToPG(actor.UserID),
			}); err != nil {
				writeError(w, http.StatusInternalServerError, errors.New("create api key permission"))
				return
			}
		}
	}
	summary, err := apiKeySummaryFromRecord(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("format api key"))
		return
	}
	summary.Permissions = displayAPIKeyPermissionGrants(permissionGrants)
	writeJSON(w, http.StatusCreated, api.APIKeyIssued{APIKeySummary: summary, RawKey: generated.Raw})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := ids.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("api key not found"))
		return
	}
	actor := actorFromContext(r.Context())
	rows, err := s.db.RevokeAPIKey(r.Context(), db.RevokeAPIKeyParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(id),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("revoke api key"))
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, errors.New("api key not found"))
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

type normalizedAPIKeyPermissionGrant struct {
	display       api.APIKeyPermissionGrant
	projectID     pgtype.UUID
	environmentID pgtype.UUID
}

func (s *Server) validateAPIKeyPermissionGrants(ctx context.Context, orgID uuid.UUID, grants []api.APIKeyPermissionGrant) ([]normalizedAPIKeyPermissionGrant, error) {
	if len(grants) == 0 {
		return nil, errors.New("permissions must include at least one grant")
	}
	normalized := make([]normalizedAPIKeyPermissionGrant, 0, len(grants))
	for _, grant := range grants {
		if len(grant.Scopes) == 0 {
			return nil, errors.New("permission grants must include at least one scope")
		}
		scopedScopes := make([]api.APIKeyScope, 0, len(grant.Scopes))
		orgScopes := make([]api.APIKeyScope, 0, len(grant.Scopes))
		seen := map[api.APIKeyScope]struct{}{}
		for _, scope := range grant.Scopes {
			normalizedScope, ok := normalizeAPIKeyScope(scope)
			if !ok {
				return nil, fmt.Errorf("unsupported permission scope %q", scope)
			}
			if _, ok := seen[normalizedScope]; ok {
				continue
			}
			seen[normalizedScope] = struct{}{}
			if apiKeyScopeIsOrgLevel(normalizedScope) {
				orgScopes = append(orgScopes, normalizedScope)
			} else {
				scopedScopes = append(scopedScopes, normalizedScope)
			}
		}
		if len(scopedScopes) > 0 {
			projectID := strings.TrimSpace(grant.ProjectID)
			if projectID == "" {
				projectID = auth.DefaultProjectID
			}
			environmentID := strings.TrimSpace(grant.EnvironmentID)
			if environmentID == "" {
				environmentID = auth.DefaultEnvironmentID
			}
			scope, projectUUID, environmentUUID, err := s.normalizeProjectEnvironmentScope(ctx, orgID, projectID, environmentID)
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, normalizedAPIKeyPermissionGrant{
				display: api.APIKeyPermissionGrant{
					ProjectID:     scope.ProjectID,
					EnvironmentID: scope.EnvironmentID,
					Scopes:        scopedScopes,
				},
				projectID:     projectUUID,
				environmentID: environmentUUID,
			})
		}
		if len(orgScopes) > 0 {
			normalized = append(normalized, normalizedAPIKeyPermissionGrant{
				display: api.APIKeyPermissionGrant{
					ProjectID:     auth.DefaultProjectID,
					EnvironmentID: auth.DefaultEnvironmentID,
					Scopes:        orgScopes,
				},
			})
		}
	}
	return normalized, nil
}

func displayAPIKeyPermissionGrants(grants []normalizedAPIKeyPermissionGrant) []api.APIKeyPermissionGrant {
	display := make([]api.APIKeyPermissionGrant, 0, len(grants))
	for _, grant := range grants {
		display = append(display, grant.display)
	}
	return display
}

func apiKeyScopeIsOrgLevel(scope api.APIKeyScope) bool {
	return scope == api.APIKeyScopeWaitpointPolicies
}

func normalizeAPIKeyScope(scope api.APIKeyScope) (api.APIKeyScope, bool) {
	switch strings.TrimSpace(string(scope)) {
	case string(api.APIKeyScopeRunsCreate):
		return api.APIKeyScopeRunsCreate, true
	case string(api.APIKeyScopeRunsRead):
		return api.APIKeyScopeRunsRead, true
	case string(api.APIKeyScopeWaitpointPolicies):
		return api.APIKeyScopeWaitpointPolicies, true
	case string(api.APIKeyScopeWaitpointsRespond):
		return api.APIKeyScopeWaitpointsRespond, true
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
	case api.APIKeyScopeWaitpointPolicies:
		return auth.PermissionWaitpointPolicies, true
	case api.APIKeyScopeWaitpointsRespond:
		return auth.PermissionWaitpointsRespond, true
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
	case string(auth.PermissionWaitpointPolicies):
		return api.APIKeyScopeWaitpointPolicies, true
	case string(auth.PermissionWaitpointsRespond):
		return api.APIKeyScopeWaitpointsRespond, true
	case string(auth.PermissionSecretsWrite):
		return api.APIKeyScopeSecretsWrite, true
	case string(auth.PermissionTasksDeploy):
		return api.APIKeyScopeTasksDeploy, true
	default:
		return "", false
	}
}

func apiKeyPermissionGrantsFromRows(rows []db.ApiKeyGrant) []api.APIKeyPermissionGrant {
	type grantKey struct {
		projectID     string
		environmentID string
	}
	byScope := map[grantKey][]api.APIKeyScope{}
	order := make([]grantKey, 0, len(rows))
	for _, row := range rows {
		key := grantKey{
			projectID:     apiKeyScopeID(row.ProjectID, auth.DefaultProjectID),
			environmentID: apiKeyScopeID(row.EnvironmentID, auth.DefaultEnvironmentID),
		}
		if _, ok := byScope[key]; !ok {
			order = append(order, key)
		}
		scope, ok := apiKeyPermissionScope(row.Permission)
		if !ok {
			continue
		}
		byScope[key] = append(byScope[key], scope)
	}
	grants := make([]api.APIKeyPermissionGrant, 0, len(order))
	for _, key := range order {
		if len(byScope[key]) == 0 {
			continue
		}
		grants = append(grants, api.APIKeyPermissionGrant{
			ProjectID:     key.projectID,
			EnvironmentID: key.environmentID,
			Scopes:        byScope[key],
		})
	}
	return grants
}

func apiKeyScopeID(value pgtype.UUID, fallback string) string {
	if !value.Valid {
		return fallback
	}
	return ids.MustFromPG(value).String()
}

func apiKeySummaryFromRecord(record db.APIKey) (api.APIKeySummary, error) {
	return apiKeySummary(
		record.ID,
		record.Name,
		record.KeyPrefix,
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
		row.CreatedAt,
		row.LastUsedAt,
		row.ExpiresAt,
		row.RevokedAt,
	)
}

func apiKeySummary(id pgtype.UUID, name string, keyPrefix string, createdAt pgtype.Timestamptz, lastUsedAt pgtype.Timestamptz, expiresAt pgtype.Timestamptz, revokedAt pgtype.Timestamptz) (api.APIKeySummary, error) {
	parsedID, err := ids.FromPG(id)
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
		ID:         parsedID.String(),
		Name:       name,
		KeyPrefix:  keyPrefix,
		Status:     status,
		CreatedAt:  pgTime(createdAt),
		LastUsedAt: pgTimePtr(lastUsedAt),
		ExpiresAt:  pgTimePtr(expiresAt),
		RevokedAt:  pgTimePtr(revokedAt),
	}, nil
}

func pgTimePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	valueTime := value.Time
	return &valueTime
}
