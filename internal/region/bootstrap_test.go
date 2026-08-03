package region

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestEnsureRegionCreatesConfiguredRegion(t *testing.T) {
	store := &bootstrapStore{}

	if err := Ensure(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "use1",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
	}); err != nil {
		t.Fatal(err)
	}
	if store.regionID != "use1" {
		t.Fatalf("ensured region = %q", store.regionID)
	}
}

func TestEnsureRegionChecksConfiguredDefaultRegion(t *testing.T) {
	store := &bootstrapStore{defaultRegion: db.Region{ID: "iad", State: db.RegionStateAvailable}}

	if err := Ensure(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "iad",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
	}); err != nil {
		t.Fatal(err)
	}
	if !store.gotDefaultRegion {
		t.Fatal("default region was not checked")
	}
}

func TestEnsureRegionRejectsMissingConfiguredDefaultRegion(t *testing.T) {
	store := &bootstrapStore{defaultErr: pgx.ErrNoRows}

	err := Ensure(context.Background(), store, BootstrapConfig{
		RegionID:          "use1",
		DefaultRegionID:   "iad",
		Provider:          "aws",
		ProviderRegion:    "us-east-1",
		RegionDisplayName: "US East",
	})
	if err == nil || !strings.Contains(err.Error(), "get default region") {
		t.Fatalf("err = %v, want default region lookup failure", err)
	}
}

func TestEnsureRegionRejectsInvalidIdentifiersBeforeStorage(t *testing.T) {
	for name, configure := range map[string]func(*BootstrapConfig){
		"region": func(cfg *BootstrapConfig) {
			cfg.RegionID = strings.Repeat("r", MaxIDBytes+1)
		},
		"default region": func(cfg *BootstrapConfig) {
			cfg.DefaultRegionID = "default\nregion"
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &bootstrapStore{}
			cfg := BootstrapConfig{
				RegionID:          strings.Repeat("r", MaxIDBytes),
				DefaultRegionID:   strings.Repeat("r", MaxIDBytes),
				Provider:          "test",
				ProviderRegion:    "test",
				RegionDisplayName: "Test",
			}
			configure(&cfg)
			if err := Ensure(context.Background(), store, cfg); err == nil {
				t.Fatal("Ensure returned nil error")
			}
			if store.regionID != "" {
				t.Fatalf("stored invalid region %q", store.regionID)
			}
		})
	}
}

type bootstrapStore struct {
	regionID         string
	defaultRegion    db.Region
	defaultErr       error
	gotDefaultRegion bool
}

func (s *bootstrapStore) EnsureRegion(_ context.Context, arg db.EnsureRegionParams) (db.Region, error) {
	s.regionID = arg.ID
	return db.Region{ID: arg.ID, State: db.RegionStateAvailable}, nil
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
