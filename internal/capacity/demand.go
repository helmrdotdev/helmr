package capacity

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type queuedDemandStore interface {
	GetWorkerGroup(context.Context, pgtype.UUID) (db.WorkerGroup, error)
	ListQueuedRunEligibleScopes(context.Context, db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error)
	ListPendingWorkspaceExecCapacityCandidates(context.Context, db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error)
}

func HasQueuedDemand(ctx context.Context, store queuedDemandStore, workerGroupID uuid.UUID) (bool, error) {
	group, err := store.GetWorkerGroup(ctx, pgvalue.UUID(workerGroupID))
	if err != nil {
		return false, fmt.Errorf("get Worker Group: %w", err)
	}
	runs, err := store.ListQueuedRunEligibleScopes(ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 1, RegionFilter: group.RegionID,
	})
	if err != nil {
		return false, fmt.Errorf("list eligible queued runs: %w", err)
	}
	if len(runs) != 0 {
		return true, nil
	}
	execs, err := store.ListPendingWorkspaceExecCapacityCandidates(ctx, db.ListPendingWorkspaceExecCapacityCandidatesParams{
		RegionID: group.RegionID, RowLimit: 1,
	})
	if err != nil {
		return false, fmt.Errorf("list pending Workspace Execs: %w", err)
	}
	return len(execs) != 0, nil
}
