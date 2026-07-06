package workergroup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestBootstrapEnsuresRegionWorkerGroupAndHealth(t *testing.T) {
	store := &bootstrapStore{}

	if err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "use1",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		WorkerGroupID:     "use1-worker-group-1",
	}); err != nil {
		t.Fatal(err)
	}
	if store.workerGroupID != "use1-worker-group-1" || store.regionID != "use1" {
		t.Fatalf("bootstrapped worker group = %q region = %q", store.workerGroupID, store.regionID)
	}
	if store.healthState != db.WorkerGroupHealthStateHealthy {
		t.Fatalf("health state = %s, want healthy", store.healthState)
	}
}

func TestBootstrapChecksConfiguredDefaultRegion(t *testing.T) {
	store := &bootstrapStore{defaultRegion: db.Region{ID: "iad", State: db.RegionStateAvailable}}

	if err := Bootstrap(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "iad",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
		WorkerGroupID:     "use1-worker-group-1",
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
		WorkerGroupID:     "use1-worker-group-1",
	})
	if err == nil || !strings.Contains(err.Error(), "get default region") {
		t.Fatalf("err = %v, want default region lookup failure", err)
	}
}

type bootstrapStore struct {
	workerGroupID    string
	regionID         string
	defaultRegion    db.Region
	defaultErr       error
	gotDefaultRegion bool
	healthState      db.WorkerGroupHealthState
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

func (s *bootstrapStore) EnsureDefaultWorkerGroup(_ context.Context, arg db.EnsureDefaultWorkerGroupParams) (db.WorkerGroup, error) {
	s.workerGroupID = arg.ID
	s.regionID = arg.RegionID
	return db.WorkerGroup{ID: arg.ID, RegionID: arg.RegionID}, nil
}

func (s *bootstrapStore) ReportWorkerGroupHealth(_ context.Context, arg db.ReportWorkerGroupHealthParams) (db.WorkerGroup, error) {
	s.healthState = arg.HealthState
	return db.WorkerGroup{ID: arg.WorkerGroupID, HealthState: arg.HealthState}, nil
}
