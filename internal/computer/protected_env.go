package computer

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

// ProtectedEnv is what a guest receives for a Computer's protected
// bindings: inert placeholders keyed by environment variable and the public
// CA certificate of the Computer's secret proxy.
type ProtectedEnv struct {
	Env map[string]string
	CA  []byte
}

// ReadProtectedEnv returns the Computer's protected environment, or nil when
// it has no protected binding. It fails when the persisted CA is invalid or
// expired.
func ReadProtectedEnv(ctx context.Context, q db.Querier, environmentID, computerID pgtype.UUID) (*ProtectedEnv, error) {
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
	return &ProtectedEnv{Env: env, CA: trust.Certificate}, nil
}
