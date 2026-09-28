package controlplane

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

// Guest delivery carries only public CA and inert Computer selectors.
func computerProtectedEnv(ctx context.Context, q db.Querier, environmentID, computerID pgtype.UUID) (*workerapi.ProtectedEnv, error) {
	bindings, err := q.ListComputerSecrets(ctx, computerID)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, binding := range bindings {
		if binding.Mode == "protected" {
			env[binding.PlacementTarget] = binding.Placeholder
		}
	}
	if len(env) == 0 {
		return nil, nil
	}
	trust, err := q.GetComputerSecretCAPublic(ctx, db.GetComputerSecretCAPublicParams{EnvironmentID: environmentID, ComputerID: computerID})
	if err != nil {
		return nil, err
	}
	if err := secret.ValidateProxyTrust(trust.Certificate, trust.NotAfter.Time, time.Now()); err != nil {
		return nil, err
	}
	return &workerapi.ProtectedEnv{Env: env, CA: trust.Certificate}, nil
}
