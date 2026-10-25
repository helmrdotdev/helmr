package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func authorizeOperation(ctx context.Context, q db.DBTX, p auth.Principal, scope Scope, permission auth.Permission) error {
	s := auth.Scope{OrgID: scope.OrgID, ProjectID: scope.ProjectID.String(), EnvironmentID: scope.EnvironmentID.String()}
	current, err := access.Current(ctx, q, p, s)
	if errors.Is(err, access.ErrDenied) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !current.HasPermission(permission, s) {
		return ErrDenied
	}
	return nil
}

func operationActor(ctx context.Context, q db.DBTX, p auth.Principal, scope Scope, permission auth.Permission) (audit.Actor, error) {
	s := auth.Scope{OrgID: scope.OrgID, ProjectID: scope.ProjectID.String(), EnvironmentID: scope.EnvironmentID.String()}
	current, err := access.Current(ctx, q, p, s)
	if err != nil {
		return audit.Actor{}, err
	}
	if !current.HasPermission(permission, s) {
		return audit.Actor{}, ErrDenied
	}
	return audit.Authorized(current, permission, s)
}
