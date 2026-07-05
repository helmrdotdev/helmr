package cell

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrPlacementOutsideLocalCell = errors.New("route resolves outside local cell")
	ErrRouteUnavailable          = errors.New("environment route is unavailable")
)

type PlacementStore interface {
	GetRoutableEnvironmentCellRoute(context.Context, db.GetRoutableEnvironmentCellRouteParams) (db.GetRoutableEnvironmentCellRouteRow, error)
}

type RouteStore interface {
	DrainActiveEnvironmentCellRoutes(context.Context, db.DrainActiveEnvironmentCellRoutesParams) error
	GetOrgCellRouteTarget(context.Context, db.GetOrgCellRouteTargetParams) (db.GetOrgCellRouteTargetRow, error)
	EnsureEnvironmentCellRoute(context.Context, db.EnsureEnvironmentCellRouteParams) (db.EnsureEnvironmentCellRouteRow, error)
}

type ResolvePlacementParams struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	RegionID      string
	LocalCellID   string
}

type Placement struct {
	CellID          string
	RegionID        string
	RouteGeneration int64
}

func ResolvePlacement(ctx context.Context, store PlacementStore, params ResolvePlacementParams) (Placement, error) {
	if store == nil {
		return Placement{}, errors.New("placement store is required")
	}
	regionID := strings.TrimSpace(params.RegionID)
	localCellID := strings.TrimSpace(params.LocalCellID)
	if regionID == "" {
		return Placement{}, errors.New("placement region id is required")
	}
	if localCellID == "" {
		return Placement{}, errors.New("local cell id is required")
	}
	route, err := store.GetRoutableEnvironmentCellRoute(ctx, db.GetRoutableEnvironmentCellRouteParams{
		OrgID:         params.OrgID,
		ProjectID:     params.ProjectID,
		EnvironmentID: params.EnvironmentID,
		RegionID:      regionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, ErrRouteUnavailable
	}
	if err != nil {
		return Placement{}, fmt.Errorf("resolve environment route: %w", err)
	}
	if route.CellID != localCellID {
		return Placement{}, ErrPlacementOutsideLocalCell
	}
	return Placement{
		CellID:          route.CellID,
		RegionID:        route.RegionID,
		RouteGeneration: route.RouteGeneration,
	}, nil
}

type EnsureEnvironmentRouteParams struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	RegionID      string
	LocalCellID   string
}

func EnsureEnvironmentRoute(ctx context.Context, store RouteStore, params EnsureEnvironmentRouteParams) (db.EnsureEnvironmentCellRouteRow, error) {
	if store == nil {
		return db.EnsureEnvironmentCellRouteRow{}, errors.New("route store is required")
	}
	regionID := strings.TrimSpace(params.RegionID)
	localCellID := strings.TrimSpace(params.LocalCellID)
	if regionID == "" {
		return db.EnsureEnvironmentCellRouteRow{}, errors.New("route region id is required")
	}
	if localCellID == "" {
		return db.EnsureEnvironmentCellRouteRow{}, errors.New("local cell id is required")
	}
	_, err := store.GetOrgCellRouteTarget(ctx, db.GetOrgCellRouteTargetParams{
		OrgID:    params.OrgID,
		RegionID: regionID,
		CellID:   localCellID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.EnsureEnvironmentCellRouteRow{}, ErrRouteUnavailable
	}
	if err != nil {
		return db.EnsureEnvironmentCellRouteRow{}, fmt.Errorf("load route target: %w", err)
	}
	if err := store.DrainActiveEnvironmentCellRoutes(ctx, db.DrainActiveEnvironmentCellRoutesParams{
		OrgID:         params.OrgID,
		ProjectID:     params.ProjectID,
		EnvironmentID: params.EnvironmentID,
		RegionID:      regionID,
		CellID:        localCellID,
	}); err != nil {
		return db.EnsureEnvironmentCellRouteRow{}, fmt.Errorf("drain previous active environment routes: %w", err)
	}
	return store.EnsureEnvironmentCellRoute(ctx, db.EnsureEnvironmentCellRouteParams{
		OrgID:         params.OrgID,
		ProjectID:     params.ProjectID,
		EnvironmentID: params.EnvironmentID,
		RegionID:      regionID,
		CellID:        localCellID,
		RouteState:    db.EnvironmentCellRouteStateActive,
	})
}
