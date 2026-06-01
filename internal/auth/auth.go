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
	"github.com/helmrdotdev/helmr/internal/ids"
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
	OrgID       uuid.UUID
	UserID      uuid.UUID
	APIKeyID    uuid.UUID
	SessionID   uuid.UUID
	Kind        ActorKind
	Role        Role
	Permissions []PermissionGrant
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
	orgID, err := ids.FromPG(row.OrgID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key org id: %w", err)
	}
	grants, err := permissionGrantsFromAPIKey(row.Grants)
	if err != nil {
		return Actor{}, fmt.Errorf("api key grants: %w", err)
	}
	apiKeyID, err := ids.FromPG(row.ID)
	if err != nil {
		return Actor{}, fmt.Errorf("api key id: %w", err)
	}
	return Actor{OrgID: orgID, APIKeyID: apiKeyID, Kind: ActorKindAPIKey, Role: Role(row.Role), Permissions: grants}, nil
}

func HashAPIKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type apiKeyGrantRow struct {
	Permission    string  `json:"permission"`
	ProjectID     *string `json:"project_id"`
	EnvironmentID *string `json:"environment_id"`
}

func permissionGrantsFromAPIKey(rawValue any) ([]PermissionGrant, error) {
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
	type grantKey struct {
		projectID     string
		environmentID string
	}
	byScope := map[grantKey][]Permission{}
	order := make([]grantKey, 0, len(rows))
	for _, row := range rows {
		key := grantKey{}
		if row.ProjectID != nil {
			key.projectID = strings.TrimSpace(*row.ProjectID)
		}
		if row.EnvironmentID != nil {
			key.environmentID = strings.TrimSpace(*row.EnvironmentID)
		}
		if _, ok := byScope[key]; !ok {
			order = append(order, key)
		}
		permissions := normalizeAPIKeyGrantPermission(row.Permission)
		if len(permissions) == 0 {
			continue
		}
		byScope[key] = append(byScope[key], permissions...)
	}
	grants := make([]PermissionGrant, 0, len(order))
	for _, key := range order {
		grants = append(grants, PermissionGrant{
			ProjectID:     key.projectID,
			EnvironmentID: key.environmentID,
			Permissions:   byScope[key],
		})
	}
	return grants, nil
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
	case string(PermissionWaitpointPolicies):
		return []Permission{PermissionWaitpointPolicies}
	case string(PermissionRunsCreate):
		return []Permission{PermissionRunsCreate}
	case string(PermissionRunsRead):
		return []Permission{PermissionRunsRead}
	case string(PermissionWaitpointsRespond):
		return []Permission{PermissionWaitpointsRespond}
	case string(PermissionSecretsUse):
		return []Permission{PermissionSecretsUse}
	case string(PermissionSecretsWrite):
		return []Permission{PermissionSecretsWrite}
	case string(PermissionTasksDeploy):
		return []Permission{PermissionTasksDeploy}
	default:
		return nil
	}
}
