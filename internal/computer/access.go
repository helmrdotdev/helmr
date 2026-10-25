package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
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
