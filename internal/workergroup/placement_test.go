package workergroup

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestResolvePlacementRejectsMissingWorkerGroup(t *testing.T) {
	store := placementStore{err: pgx.ErrNoRows}

	_, err := ResolvePlacement(context.Background(), store, ResolvePlacementParams{
		OrgID:     pgvalue.UUID(testUUID(1)),
		ProjectID: pgvalue.UUID(testUUID(2)),
	})
	if !errors.Is(err, ErrPlacementUnavailable) {
		t.Fatalf("err = %v, want ErrPlacementUnavailable", err)
	}
}

func TestResolvePlacementReturnsSelectedWorkerGroup(t *testing.T) {
	store := placementStore{
		row: db.SelectProjectPlacementWorkerGroupRow{
			WorkerGroupID: "use1-worker-group-1",
			RegionID:      "use1",
		},
	}

	placement, err := ResolvePlacement(context.Background(), store, ResolvePlacementParams{
		OrgID:     pgvalue.UUID(testUUID(1)),
		ProjectID: pgvalue.UUID(testUUID(2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if placement.WorkerGroupID != "use1-worker-group-1" || placement.RegionID != "use1" {
		t.Fatalf("placement = %+v", placement)
	}
}

type placementStore struct {
	row db.SelectProjectPlacementWorkerGroupRow
	err error
}

func (s placementStore) SelectProjectPlacementWorkerGroup(context.Context, db.SelectProjectPlacementWorkerGroupParams) (db.SelectProjectPlacementWorkerGroupRow, error) {
	return s.row, s.err
}

func testUUID(suffix byte) uuid.UUID {
	return uuid.UUID{15: suffix}
}
