package region

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
)

type RegionStore interface {
	EnsureRegion(context.Context, db.EnsureRegionParams) (db.Region, error)
	GetRegion(context.Context, string) (db.Region, error)
}

type BootstrapConfig struct {
	RegionID          string
	DefaultRegionID   string
	Provider          string
	ProviderRegion    string
	RegionDisplayName string
}

func Ensure(ctx context.Context, store RegionStore, cfg BootstrapConfig) error {
	if store == nil {
		return errors.New("region bootstrap store is required")
	}
	regionID := strings.TrimSpace(cfg.RegionID)
	provider := strings.TrimSpace(cfg.Provider)
	providerRegion := strings.TrimSpace(cfg.ProviderRegion)
	displayName := strings.TrimSpace(cfg.RegionDisplayName)
	defaultRegionID := strings.TrimSpace(cfg.DefaultRegionID)
	if regionID == "" {
		return errors.New("region id is required")
	}
	if defaultRegionID == "" {
		return errors.New("default region id is required")
	}
	if provider == "" {
		return errors.New("provider is required")
	}
	if providerRegion == "" {
		return errors.New("provider region is required")
	}
	if displayName == "" {
		displayName = regionID
	}
	region, err := store.EnsureRegion(ctx, db.EnsureRegionParams{
		ID:             regionID,
		Provider:       provider,
		ProviderRegion: providerRegion,
		DisplayName:    displayName,
		State:          db.RegionStateAvailable,
		Visibility:     db.RegionVisibilityPublic,
		Location:       "",
		StaticIps:      []string{},
	})
	if err != nil {
		return fmt.Errorf("ensure region: %w", err)
	}
	if region.State != db.RegionStateAvailable {
		return fmt.Errorf("region %q is not available", regionID)
	}
	if defaultRegionID != regionID {
		defaultRegion, err := store.GetRegion(ctx, defaultRegionID)
		if err != nil {
			return fmt.Errorf("get default region: %w", err)
		}
		if defaultRegion.State != db.RegionStateAvailable {
			return fmt.Errorf("default region %q is not available", defaultRegionID)
		}
	}
	return nil
}
