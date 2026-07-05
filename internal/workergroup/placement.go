package workergroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrPlacementUnavailable = errors.New("environment placement is unavailable")
)

type PlacementStore interface {
	SelectProjectPlacementWorkerGroup(context.Context, db.SelectProjectPlacementWorkerGroupParams) (db.SelectProjectPlacementWorkerGroupRow, error)
}

type ResolvePlacementParams struct {
	OrgID     pgtype.UUID
	ProjectID pgtype.UUID
}

type Placement struct {
	WorkerGroupID string
	RegionID      string
}

func ResolvePlacement(ctx context.Context, store PlacementStore, params ResolvePlacementParams) (Placement, error) {
	if store == nil {
		return Placement{}, errors.New("placement store is required")
	}
	placement, err := store.SelectProjectPlacementWorkerGroup(ctx, db.SelectProjectPlacementWorkerGroupParams{
		OrgID:     params.OrgID,
		ProjectID: params.ProjectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, ErrPlacementUnavailable
	}
	if err != nil {
		return Placement{}, fmt.Errorf("resolve environment placement: %w", err)
	}
	return Placement{
		WorkerGroupID: placement.WorkerGroupID,
		RegionID:      placement.RegionID,
	}, nil
}
