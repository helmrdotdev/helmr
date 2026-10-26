package computer

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func operationActor(ctx context.Context, q db.DBTX, p auth.Principal, scope Scope, permission auth.Permission) (audit.Evidence, error) {
	s := auth.Scope{OrgID: scope.OrgID, ProjectID: scope.ProjectID.String(), EnvironmentID: scope.EnvironmentID.String()}
	current, err := access.Current(ctx, q, p, s)
	if err != nil {
		return audit.Evidence{}, err
	}
	if permissionErr := auth.Require(current, permission, s,
		ErrDenied); permissionErr != nil {
		return audit.Evidence{}, permissionErr
	}
	return audit.Authorized(current, permission, s)
}
