package control

import (
	"context"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

type BootstrapStore interface {
	EnsureDefaultOrganization(ctx context.Context, id pgtype.UUID) error
	OwnerExists(ctx context.Context, orgID pgtype.UUID) (bool, error)
}

type WorkerRegistrationTokenStore interface {
	EnsureDefaultWorkerRegistrationToken(ctx context.Context, arg db.EnsureDefaultWorkerRegistrationTokenParams) (db.EnsureDefaultWorkerRegistrationTokenRow, error)
}

type BootstrapResult struct {
	SetupRequired bool
}

func Bootstrap(ctx context.Context, queries BootstrapStore, setupEnabled bool) (BootstrapResult, error) {
	if err := queries.EnsureDefaultOrganization(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		return BootstrapResult{}, err
	}
	if !setupEnabled {
		return BootstrapResult{}, nil
	}
	ownerExists, err := queries.OwnerExists(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		return BootstrapResult{}, err
	}
	if ownerExists {
		return BootstrapResult{}, nil
	}
	return BootstrapResult{SetupRequired: true}, nil
}

func EnsureDefaultWorkerRegistrationToken(ctx context.Context, queries WorkerRegistrationTokenStore, authSecret string, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	tokenHash, err := auth.HashToken([]byte(authSecret), token)
	if err != nil {
		return fmt.Errorf("hash worker registration token: %w", err)
	}
	if _, err := queries.EnsureDefaultWorkerRegistrationToken(ctx, db.EnsureDefaultWorkerRegistrationTokenParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		TokenHash: tokenHash,
	}); err != nil {
		return err
	}
	return nil
}
