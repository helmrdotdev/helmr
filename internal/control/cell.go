package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	cellpkg "github.com/helmrdotdev/helmr/internal/cell"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type environmentPlacementStore interface {
	cellpkg.PlacementStore
	GetEnvironment(context.Context, db.GetEnvironmentParams) (db.Environment, error)
}

func (s *Server) resolveEnvironmentPlacement(ctx context.Context, store environmentPlacementStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (cellpkg.Placement, error) {
	environment, err := store.GetEnvironment(ctx, db.GetEnvironmentParams{
		OrgID:     pgvalue.UUID(orgID),
		ProjectID: projectID,
		ID:        environmentID,
	})
	if isNoRows(err) {
		return cellpkg.Placement{}, notFound(errors.New("environment not found"))
	}
	if err != nil {
		return cellpkg.Placement{}, fmt.Errorf("load environment placement: %w", err)
	}
	placement, err := cellpkg.ResolvePlacement(ctx, store, cellpkg.ResolvePlacementParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RegionID:      environment.DefaultRegionID,
		LocalCellID:   s.cellID,
	})
	if err == nil {
		return placement, nil
	}
	if errors.Is(err, cellpkg.ErrRouteUnavailable) {
		return cellpkg.Placement{}, unavailable(errors.New("environment route is unavailable"))
	}
	if errors.Is(err, cellpkg.ErrPlacementOutsideLocalCell) {
		return cellpkg.Placement{}, unavailable(errors.New("environment route belongs to a different cell"))
	}
	return cellpkg.Placement{}, fmt.Errorf("resolve environment route: %w", err)
}
