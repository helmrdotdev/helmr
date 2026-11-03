package session_test

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPlacementEditor edits placements as the Control Plane itself.
var testPlacementEditor = workergroup.PlacementEditor{
	Policy: auditpolicy.Policy{RetentionSeconds: 3600},
	Authorize: func(context.Context, pgx.Tx, bool) (audit.Evidence, error) {
		return audit.Internal(audit.ControlPlane), nil
	},
}

// groupOwnerOperator operates the group as its owner, the organization for an
// organization group, as the Control Plane itself.
func groupOwnerOperator(t *testing.T, pool *pgxpool.Pool, group uuid.UUID) workergroup.Operator {
	t.Helper()
	var owner *uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT owner_org_id FROM worker_groups WHERE id=$1`, group).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return workergroup.DeploymentOperator(audit.Internal(audit.ControlPlane))
	}
	return workergroup.OrganizationOperator(*owner, func(context.Context, pgx.Tx) (audit.Evidence, error) {
		return audit.Internal(audit.ControlPlane), nil
	})
}
