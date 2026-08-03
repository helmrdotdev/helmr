package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type dbAuthenticator struct {
	db db.Querier
}

func NewDBAuthenticator(database db.Querier) auth.Authenticator {
	return dbAuthenticator{db: database}
}

func (a dbAuthenticator) Authenticate(ctx context.Context, bearerToken string) (auth.Actor, error) {
	token := strings.TrimSpace(bearerToken)
	if token == "" {
		return auth.Actor{}, auth.ErrUnauthenticated
	}
	row, err := a.db.TouchActiveAPIKeyByTokenHash(ctx, auth.HashAPIKey(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Actor{}, auth.ErrUnauthenticated
		}
		return auth.Actor{}, fmt.Errorf("verify api key: %w", err)
	}
	orgID, err := pgvalue.UUIDValue(row.OrgID)
	if err != nil {
		return auth.Actor{}, fmt.Errorf("api key org id: %w", err)
	}
	projectID, err := pgvalue.UUIDValue(row.ProjectID)
	if err != nil {
		return auth.Actor{}, fmt.Errorf("api key project id: %w", err)
	}
	environmentID, err := pgvalue.UUIDValue(row.EnvironmentID)
	if err != nil {
		return auth.Actor{}, fmt.Errorf("api key environment id: %w", err)
	}
	permissions, err := permissionsFromAPIKey(row.Grants)
	if err != nil {
		return auth.Actor{}, fmt.Errorf("api key grants: %w", err)
	}
	apiKeyID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return auth.Actor{}, fmt.Errorf("api key id: %w", err)
	}
	return auth.Actor{
		OrgID:         orgID,
		APIKeyID:      apiKeyID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.Role(row.Role),
		Permissions:   permissions,
	}, nil
}

type apiKeyGrantRow struct {
	Permission string `json:"permission"`
}

func permissionsFromAPIKey(rawValue any) ([]auth.Permission, error) {
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
	permissions := make([]auth.Permission, 0, len(rows))
	for _, row := range rows {
		permission, ok := auth.ParseAPIKeyGrant(row.Permission)
		if ok {
			permissions = append(permissions, permission)
		}
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
