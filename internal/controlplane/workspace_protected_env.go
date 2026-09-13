package controlplane

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
	"time"
)

// Guest delivery carries only public CA and inert Workspace selectors.
func workspaceProtectedEnv(ctx context.Context, q db.Querier, environmentID, workspaceID pgtype.UUID) (*workerapi.ProtectedEnv, error) {
	bindings, err := q.ListWorkspaceSecrets(ctx, workspaceID)
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
	trust, err := q.GetWorkspaceProxyTrust(ctx, db.GetWorkspaceProxyTrustParams{EnvironmentID: environmentID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	if err := secret.ValidateProxyTrust(trust, time.Now()); err != nil {
		return nil, err
	}
	return &workerapi.ProtectedEnv{Env: env, CA: trust.Certificate}, nil
}
