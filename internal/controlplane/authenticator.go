package controlplane

import (
	"context"
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
	permissions := permissionsFromAPIKey(row.Permissions)
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

func permissionsFromAPIKey(values []string) []auth.Permission {
	permissions := make([]auth.Permission, 0, len(values))
	for _, value := range values {
		permission, ok := auth.ParseAPIKeyGrant(value)
		if ok {
			permissions = append(permissions, permission)
		}
	}
	if len(permissions) == 0 {
		return nil
	}
	return permissions
}
