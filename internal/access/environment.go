package access

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"uuid"
)

func HumanInEnvironment(ctx context.Context, q db.DBTX, p auth.Principal, env uuid.UUID) (auth.Principal, error) {
	var org, project uuid.UUID
	if err := q.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, env).Scan(&org, &project); err != nil {
		return p, humanReadError(err)
	}
	return Current(ctx, q, p, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: env.String()})
}
