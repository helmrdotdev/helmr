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
	all := p.HasPermission(auth.PermissionDeploymentMetadataRead, scope) || p.HasPermission(auth.CapabilityAgentsUse, scope)
	ids := []uuid.UUID{}
	for _, g := range p.Grants {
		if g.ProjectID == scope.ProjectID && g.EnvironmentID == scope.EnvironmentID && g.AgentID != "" && g.Permission == auth.CapabilityAgentsUse {
			if id, err := uuid.Parse(g.AgentID); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return all, ids
}

func agentIdentityScope(p auth.Principal, scope auth.Scope) (bool, []uuid.UUID) {
	all, ids := agentReadScope(p, scope)
	if p.HasPermission(auth.CapabilityAgentsManage, scope) {
		all = true
	}
	for _, g := range p.Grants {
		if g.ProjectID == scope.ProjectID && g.EnvironmentID == scope.EnvironmentID && g.AgentID != "" && g.Permission == auth.CapabilityAgentsManage {
			if id, err := uuid.Parse(g.AgentID); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return all, ids
}
