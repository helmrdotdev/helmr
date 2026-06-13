package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type ActorKind string

const (
	ActorKindAPIKey  ActorKind = "api_key"
	ActorKindSession ActorKind = "session"
	ActorKindSystem  ActorKind = "system"
)

type Role string

const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

type Actor struct {
	OrgID         uuid.UUID
	UserID        uuid.UUID
	APIKeyID      uuid.UUID
	SessionID     uuid.UUID
	ProjectID     string
	EnvironmentID string
	Kind          ActorKind
	Role          Role
	Permissions   []Permission
}

type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (Actor, error)
}

type DBAuthenticator struct {
	db db.Querier
}

func NewDBAuthenticator(db db.Querier) DBAuthenticator {
	return DBAuthenticator{db: db}
}

func (a DBAuthenticator) Authenticate(ctx context.Context, bearerToken string) (Actor, error) {
	token := strings.TrimSpace(bearerToken)
	if token == "" {
		return Actor{}, ErrUnauthenticated
	}
	row, err := a.db.TouchActiveAPIKeyByTokenHash(ctx, HashAPIKey(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Actor{}, ErrUnauthenticated
		}
		return Actor{}, fmt.Errorf("verify api key: %w", err)
	}
	orgID, err := pgvalue.UUIDValue(row.OrgID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key org id: %w", err)
	}
	projectID, err := pgvalue.UUIDValue(row.ProjectID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key project id: %w", err)
	}
	environmentID, err := pgvalue.UUIDValue(row.EnvironmentID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key environment id: %w", err)
	}
	permissions, err := permissionsFromAPIKey(row.Grants)
	if err != nil {
		return Actor{}, fmt.Errorf("api key grants: %w", err)
	}
	apiKeyID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key id: %w", err)
	}
	return Actor{
		OrgID:         orgID,
		APIKeyID:      apiKeyID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
		Kind:          ActorKindAPIKey,
		Role:          Role(row.Role),
		Permissions:   permissions,
	}, nil
}

func (a Actor) EnvironmentScope() (Scope, bool) {
	if a.ProjectID == "" || a.EnvironmentID == "" {
		return Scope{}, false
	}
	projectID, err := uuid.Parse(a.ProjectID)
	if err != nil || projectID == uuid.Nil {
		return Scope{}, false
	}
	environmentID, err := uuid.Parse(a.EnvironmentID)
	if err != nil || environmentID == uuid.Nil {
		return Scope{}, false
	}
	return Scope{OrgID: a.OrgID, ProjectID: a.ProjectID, EnvironmentID: a.EnvironmentID}, true
}

func HashAPIKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type apiKeyGrantRow struct {
	Permission string `json:"permission"`
}

func permissionsFromAPIKey(rawValue any) ([]Permission, error) {
	raw, err := apiKeyGrantJSON(rawValue)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []apiKeyGrantRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	permissions := make([]Permission, 0, len(rows))
	for _, row := range rows {
		normalized := normalizeAPIKeyGrantPermission(row.Permission)
		if len(normalized) == 0 {
			continue
		}
		permissions = append(permissions, normalized...)
	}
	if len(permissions) == 0 {
		return nil, nil
	}
	return permissions, nil
}

func apiKeyGrantJSON(rawValue any) ([]byte, error) {
	switch raw := rawValue.(type) {
	case nil:
		return nil, nil
	case []byte:
		return raw, nil
	case string:
		return []byte(raw), nil
	case json.RawMessage:
		return []byte(raw), nil
	default:
		return nil, fmt.Errorf("unsupported grant payload type %T", rawValue)
	}
}

func normalizeAPIKeyGrantPermission(permission string) []Permission {
	switch strings.TrimSpace(permission) {
	case string(PermissionRunsCreate):
		return []Permission{PermissionRunsCreate}
	case string(PermissionRunsRead):
		return []Permission{PermissionRunsRead}
	case string(PermissionRunsManage):
		return []Permission{PermissionRunsManage}
	case string(PermissionWaitpointsRespond):
		return []Permission{PermissionWaitpointsRespond}
	case string(PermissionWaitpointPolicies):
		return []Permission{PermissionWaitpointPolicies}
	case string(PermissionSecretsWrite):
		return []Permission{PermissionSecretsWrite}
	case string(PermissionTasksDeploy):
		return []Permission{PermissionTasksDeploy}
	default:
		return nil
	}
}
