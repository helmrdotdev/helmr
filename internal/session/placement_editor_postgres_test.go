package session_test

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// testPlacementEditor edits placements as the Control Plane itself.
var testPlacementEditor = workergroup.PlacementEditor{
	Policy: auditpolicy.Policy{RetentionSeconds: 3600},
	Authorize: func(context.Context, pgx.Tx, bool) (audit.Evidence, error) {
		return audit.Internal(audit.ControlPlane), nil
	},
}
