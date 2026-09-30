package identity

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// APIKeyFilter selects API keys by status.
type APIKeyFilter string

const (
	APIKeyFilterActive  APIKeyFilter = "active"
	APIKeyFilterExpired APIKeyFilter = "expired"
	APIKeyFilterRevoked APIKeyFilter = "revoked"
	APIKeyFilterAll     APIKeyFilter = "all"
)

// ParseAPIKeyFilter parses an API key status filter; empty selects active keys.
func ParseAPIKeyFilter(value string) (APIKeyFilter, error) {
	switch filter := APIKeyFilter(value); filter {
	case "":
		return APIKeyFilterActive, nil
	case APIKeyFilterActive, APIKeyFilterExpired, APIKeyFilterRevoked, APIKeyFilterAll:
		return filter, nil
	default:
		return "", invalidInput("filter must be active, expired, revoked, or all")
	}
}

// APIKeyPosition is the sort key of the last API key on a previous page.
type APIKeyPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// APIKeyInput describes an API key to issue. Permissions are the API key
// grantable permissions it carries; a nil ExpiresInDays never expires.
type APIKeyInput struct {
	Name          string
	Permissions   []auth.Permission
	ExpiresInDays *int
}

// IssuedAPIKey is an issued API key record with its raw key, which only the
// issue response carries.
type IssuedAPIKey struct {
	Record db.APIKey
	Raw    string
}

func authorizeAPIKeyManagement(manager auth.Principal) error {
	if !manager.HasPermission(auth.PermissionAPIKeysManage, auth.Scope{OrgID: manager.OrgID}) {
		return ErrAPIKeyManagementRequired
	}
	return nil
}

// ListAPIKeys returns up to limit API keys of the environment matching the
// filter after the given position and whether more follow.
func ListAPIKeys(ctx context.Context, q db.Querier, manager auth.Principal, scope auth.Scope, filter APIKeyFilter, limit int32, after *APIKeyPosition) ([]db.ListAPIKeysRow, bool, error) {
	if err := authorizeAPIKeyManagement(manager); err != nil {
		return nil, false, err
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return nil, false, err
	}
	params := db.ListAPIKeysParams{
		OrgID:         pgvalue.UUID(manager.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		StatusFilter:  string(filter),
		RowLimit:      limit + 1,
	}
	if after != nil {
		params.AfterCreatedAt = pgvalue.Timestamptz(after.CreatedAt)
		params.AfterID = pgvalue.UUID(after.ID)
	}
	rows, err := q.ListAPIKeys(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list api keys: %w", err)
	}
	if len(rows) > int(limit) {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// IssueAPIKey issues an API key for the environment that acts with the
// issuer's role, limited to the given permissions. An active key of the
// environment with the same name is revoked by the same statement.
func IssueAPIKey(ctx context.Context, q db.Querier, issuer auth.Principal, scope auth.Scope, input APIKeyInput) (IssuedAPIKey, error) {
	if err := authorizeAPIKeyManagement(issuer); err != nil {
		return IssuedAPIKey{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return IssuedAPIKey{}, invalidInput("name must be 1-64 characters and contain no control characters")
	}
	if len(input.Permissions) == 0 {
		return IssuedAPIKey{}, invalidInput("permissions must include at least one grant")
	}
	permissions := make([]string, 0, len(input.Permissions))
	for _, permission := range input.Permissions {
		if _, ok := auth.ParseAPIKeyGrant(string(permission)); !ok {
			return IssuedAPIKey{}, invalidInput("unsupported permission %q", permission)
		}
		permissions = append(permissions, string(permission))
	}
	expiresAt := pgtype.Timestamptz{}
	if input.ExpiresInDays != nil {
		switch *input.ExpiresInDays {
		case 30, 90, 365:
		default:
			return IssuedAPIKey{}, invalidInput("expires_in_days must be 30, 90, or 365")
		}
		expiresAt = pgvalue.Timestamptz(time.Now().AddDate(0, 0, *input.ExpiresInDays))
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return IssuedAPIKey{}, err
	}
	generated, err := auth.GenerateAPIKey()
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("generate api key: %w", err)
	}
	record, err := q.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:              pgvalue.UUID(uuid.NewV7()),
		OrgID:           pgvalue.UUID(issuer.OrgID),
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		CreatedByUserID: pgvalue.UUID(issuer.UserID),
		Role:            db.OrgMemberRole(issuer.Role),
		Permissions:     permissions,
		Name:            name,
		KeyPrefix:       generated.KeyPrefix,
		TokenHash:       generated.TokenHash,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("create api key: %w", err)
	}
	return IssuedAPIKey{Record: record, Raw: generated.Raw}, nil
}

// RevokeAPIKey revokes an API key of the environment.
func RevokeAPIKey(ctx context.Context, q db.Querier, manager auth.Principal, scope auth.Scope, id uuid.UUID) error {
	if err := authorizeAPIKeyManagement(manager); err != nil {
		return err
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return err
	}
	revoked, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{
		OrgID:         pgvalue.UUID(manager.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(id),
	})
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if revoked == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

func environmentIDs(scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("environment id: %w", err)
	}
	return pgvalue.UUID(projectID), pgvalue.UUID(environmentID), nil
}

// APIKeyAuthenticator authenticates API keys and records their use.
type APIKeyAuthenticator struct {
	q db.Querier
}

// NewAPIKeyAuthenticator returns an authenticator over the API keys in q.
func NewAPIKeyAuthenticator(q db.Querier) APIKeyAuthenticator {
	return APIKeyAuthenticator{q: q}
}

// Authenticate resolves a raw API key to its principal and records its use. An
// unknown, revoked or expired key is auth.ErrUnauthenticated.
func (a APIKeyAuthenticator) Authenticate(ctx context.Context, rawKey string) (auth.Principal, error) {
	token := strings.TrimSpace(rawKey)
	if token == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	row, err := a.q.TouchActiveAPIKeyByTokenHash(ctx, auth.HashAPIKey(token))
	if isNoRows(err) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("verify api key: %w", err)
	}
	orgID, err := pgvalue.UUIDValue(row.OrgID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key org id: %w", err)
	}
	projectID, err := pgvalue.UUIDValue(row.ProjectID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key project id: %w", err)
	}
	environmentID, err := pgvalue.UUIDValue(row.EnvironmentID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key environment id: %w", err)
	}
	apiKeyID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key id: %w", err)
	}
	return auth.Principal{
		OrgID:         orgID,
		APIKeyID:      apiKeyID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
		Kind:          auth.PrincipalKindAPIKey,
		Role:          auth.Role(row.Role),
		Permissions:   grantedPermissions(row.Permissions),
	}, nil
}

// grantedPermissions keeps the stored permissions that API keys can still be
// granted, or nil when none remain.
func grantedPermissions(values []string) []auth.Permission {
	permissions := make([]auth.Permission, 0, len(values))
	for _, value := range values {
		if permission, ok := auth.ParseAPIKeyGrant(value); ok {
			permissions = append(permissions, permission)
		}
	}
	if len(permissions) == 0 {
		return nil
	}
	return permissions
}
