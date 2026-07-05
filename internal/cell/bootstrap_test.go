package cell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestBootstrapAllowsLocalDefaultRegion(t *testing.T) {
	store := &bootstrapStore{}

	if err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "use1",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		CellID:            "cell-use1-1",
		EnvironmentClass:  "managed-cloud",
	}); err != nil {
		t.Fatal(err)
	}
	if store.cellID != "cell-use1-1" || store.regionID != "use1" {
		t.Fatalf("bootstrapped cell = %q region = %q", store.cellID, store.regionID)
	}
}

func TestBootstrapAllowsConfiguredDefaultRegionWhenCataloged(t *testing.T) {
	store := &bootstrapStore{
		defaultRegion: db.Region{ID: "iad", State: db.RegionStateAvailable},
	}

	if err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "iad",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		CellID:            "cell-use1-1",
		EnvironmentClass:  "managed-cloud",
	}); err != nil {
		t.Fatal(err)
	}
	if !store.gotDefaultRegion {
		t.Fatal("default region was not checked")
	}
}

func TestBootstrapRejectsMissingConfiguredDefaultRegion(t *testing.T) {
	store := &bootstrapStore{defaultErr: pgx.ErrNoRows}

	err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "iad",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		CellID:            "cell-use1-1",
		EnvironmentClass:  "managed-cloud",
	})
	if err == nil || !strings.Contains(err.Error(), "get default region") {
		t.Fatalf("err = %v, want default region lookup failure", err)
	}
}

func TestBootstrapRejectsCellRegionMismatch(t *testing.T) {
	store := &bootstrapStore{
		ensureCellErr: pgx.ErrNoRows,
		existingCell:  db.Cell{ID: "cell-use1-1", RegionID: "iad"},
	}

	err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "use1",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		CellID:            "cell-use1-1",
		EnvironmentClass:  "managed-cloud",
	})
	if err == nil || !strings.Contains(err.Error(), "already bound to region") {
		t.Fatalf("err = %v, want cell region mismatch", err)
	}
}

type bootstrapStore struct {
	cellID           string
	regionID         string
	defaultRegion    db.Region
	defaultErr       error
	gotDefaultRegion bool
	ensureCellErr    error
	existingCell     db.Cell
}

func (s *bootstrapStore) EnsureRegion(context.Context, db.EnsureRegionParams) (db.Region, error) {
	return db.Region{ID: "use1", State: db.RegionStateAvailable}, nil
}

func (s *bootstrapStore) GetRegion(_ context.Context, id string) (db.Region, error) {
	s.gotDefaultRegion = true
	if s.defaultErr != nil {
		return db.Region{}, s.defaultErr
	}
	if s.defaultRegion.ID == "" {
		return db.Region{}, errors.New("unexpected default region lookup")
	}
	return s.defaultRegion, nil
}

func (s *bootstrapStore) EnsureCell(_ context.Context, arg db.EnsureCellParams) (db.Cell, error) {
	if s.ensureCellErr != nil {
		return db.Cell{}, s.ensureCellErr
	}
	s.cellID = arg.ID
	s.regionID = arg.RegionID
	return db.Cell{}, nil
}

func (s *bootstrapStore) GetCell(context.Context, string) (db.Cell, error) {
	if s.existingCell.ID == "" {
		return db.Cell{}, pgx.ErrNoRows
	}
	return s.existingCell, nil
}

func (s *bootstrapStore) RefreshCellHealthFromComponents(context.Context, db.RefreshCellHealthFromComponentsParams) (db.CellHealth, error) {
	return db.CellHealth{}, nil
}

func (s *bootstrapStore) EnsureDefaultWorkerGroup(context.Context, string) (db.WorkerGroup, error) {
	return db.WorkerGroup{}, nil
}
