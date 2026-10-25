package deployment

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"uuid"
)

func currentPrincipal(ctx context.Context, q db.DBTX, p auth.Principal, scope auth.Scope) (auth.Principal, error) {
	current, err := access.Current(ctx, q, p, scope)
	if errors.Is(err, access.ErrDenied) {
		err = ErrPermissionRequired
	}
	return current, err
}
func authorizeCurrent(ctx context.Context, q db.DBTX, p auth.Principal, scope auth.Scope, permission auth.Permission) error {
	current, err := currentPrincipal(ctx, q, p, scope)
	if err != nil {
		return err
	}
	return authorize(current, scope, permission)
}

// Agent filters are applied in SQL before pagination. An empty filter stays empty.
func agentReadScope(p auth.Principal, scope auth.Scope) (bool, []uuid.UUID) {
	return p.AgentPermissionScope(auth.PermissionAgentsRead, scope)
}

func agentIdentityScope(p auth.Principal, scope auth.Scope) (bool, []uuid.UUID) {
	all := false
	ids := []uuid.UUID{}
	for _, permission := range []auth.Permission{auth.PermissionAgentsRead, auth.PermissionAgentsUse, auth.PermissionAgentsDelete, auth.PermissionAgentsRestore} {
		broad, selected := p.AgentPermissionScope(permission, scope)
		all = all || broad
		ids = append(ids, selected...)
	}
	return all, ids
}
