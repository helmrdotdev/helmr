package cell

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestResolvePlacementRejectsForeignActiveRoute(t *testing.T) {
	store := placementStore{
		route: db.EnvironmentCell{
			OrgID:           pgvalue.UUID(testUUID(1)),
			ProjectID:       pgvalue.UUID(testUUID(2)),
			EnvironmentID:   pgvalue.UUID(testUUID(3)),
			RegionID:        "us-east-1",
			CellID:          "us-east-1-cell-2",
			RouteState:      db.EnvironmentCellRouteStateActive,
			RouteGeneration: 4,
		},
	}

	_, err := ResolvePlacement(context.Background(), store, ResolvePlacementParams{
		OrgID:         pgvalue.UUID(testUUID(1)),
		ProjectID:     pgvalue.UUID(testUUID(2)),
		EnvironmentID: pgvalue.UUID(testUUID(3)),
		RegionID:      "us-east-1",
		LocalCellID:   "us-east-1-cell-1",
	})
	if !errors.Is(err, ErrPlacementOutsideLocalCell) {
		t.Fatalf("err = %v, want ErrPlacementOutsideLocalCell", err)
	}
}

func TestResolvePlacementRejectsMissingRoute(t *testing.T) {
	store := placementStore{err: pgx.ErrNoRows}

	_, err := ResolvePlacement(context.Background(), store, ResolvePlacementParams{
		OrgID:         pgvalue.UUID(testUUID(1)),
		ProjectID:     pgvalue.UUID(testUUID(2)),
		EnvironmentID: pgvalue.UUID(testUUID(3)),
		RegionID:      "us-east-1",
		LocalCellID:   "us-east-1-cell-1",
	})
	if !errors.Is(err, ErrRouteUnavailable) {
		t.Fatalf("err = %v, want ErrRouteUnavailable", err)
	}
}

func TestResolvePlacementReturnsLocalRouteGeneration(t *testing.T) {
	store := placementStore{
		route: db.EnvironmentCell{
			OrgID:           pgvalue.UUID(testUUID(1)),
			ProjectID:       pgvalue.UUID(testUUID(2)),
			EnvironmentID:   pgvalue.UUID(testUUID(3)),
			RegionID:        "us-east-1",
			CellID:          "us-east-1-cell-1",
			RouteState:      db.EnvironmentCellRouteStateActive,
			RouteGeneration: 7,
		},
	}

	placement, err := ResolvePlacement(context.Background(), store, ResolvePlacementParams{
		OrgID:         pgvalue.UUID(testUUID(1)),
		ProjectID:     pgvalue.UUID(testUUID(2)),
		EnvironmentID: pgvalue.UUID(testUUID(3)),
		RegionID:      "us-east-1",
		LocalCellID:   "us-east-1-cell-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if placement.CellID != "us-east-1-cell-1" || placement.RegionID != "us-east-1" || placement.RouteGeneration != 7 {
		t.Fatalf("placement = %+v", placement)
	}
}

type placementStore struct {
	route db.EnvironmentCell
	err   error
}

func (s placementStore) GetRoutableEnvironmentCellRoute(context.Context, db.GetRoutableEnvironmentCellRouteParams) (db.GetRoutableEnvironmentCellRouteRow, error) {
	return db.GetRoutableEnvironmentCellRouteRow{
		OrgID:           s.route.OrgID,
		ProjectID:       s.route.ProjectID,
		EnvironmentID:   s.route.EnvironmentID,
		RegionID:        s.route.RegionID,
		CellID:          s.route.CellID,
		RouteGeneration: s.route.RouteGeneration,
	}, s.err
}

func testUUID(suffix byte) uuid.UUID {
	return uuid.UUID{15: suffix}
}
