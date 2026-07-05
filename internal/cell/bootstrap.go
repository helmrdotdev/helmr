package cell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const defaultRoutingFreshness = 120 * time.Second

type BootstrapStore interface {
	EnsureRegion(context.Context, db.EnsureRegionParams) (db.Region, error)
	GetRegion(context.Context, string) (db.Region, error)
	EnsureCell(context.Context, db.EnsureCellParams) (db.Cell, error)
	GetCell(context.Context, string) (db.Cell, error)
	RefreshCellHealthFromComponents(context.Context, db.RefreshCellHealthFromComponentsParams) (db.CellHealth, error)
	EnsureDefaultWorkerGroup(context.Context, string) (db.WorkerGroup, error)
}

type BootstrapConfig struct {
	RegionID           string
	DefaultRegionID    string
	Provider           string
	ProviderRegion     string
	RegionDisplayName  string
	CellID             string
	EnvironmentClass   string
	RequiredComponents []string
}

func Bootstrap(ctx context.Context, store BootstrapStore, cfg BootstrapConfig) error {
	if store == nil {
		return errors.New("cell bootstrap store is required")
	}
	regionID := strings.TrimSpace(cfg.RegionID)
	provider := strings.TrimSpace(cfg.Provider)
	providerRegion := strings.TrimSpace(cfg.ProviderRegion)
	displayName := strings.TrimSpace(cfg.RegionDisplayName)
	cellID := strings.TrimSpace(cfg.CellID)
	environmentClass := strings.TrimSpace(cfg.EnvironmentClass)
	defaultRegionID := strings.TrimSpace(cfg.DefaultRegionID)
	requiredComponents := normalizeRequiredComponents(cfg.RequiredComponents)
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
	if cellID == "" {
		return errors.New("cell id is required")
	}
	if environmentClass == "" {
		return errors.New("cell environment class is required")
	}
	if len(requiredComponents) == 0 {
		requiredComponents = RoutingRequiredComponents()
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
	if _, err := store.EnsureCell(ctx, db.EnsureCellParams{
		ID:               cellID,
		RegionID:         regionID,
		EnvironmentClass: environmentClass,
		State:            db.CellStateActive,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := store.GetCell(ctx, cellID)
			if getErr == nil && existing.RegionID != regionID {
				return fmt.Errorf("cell %q is already bound to region %q, not %q", cellID, existing.RegionID, regionID)
			}
		}
		return fmt.Errorf("ensure cell: %w", err)
	}
	if _, err := store.RefreshCellHealthFromComponents(ctx, db.RefreshCellHealthFromComponentsParams{
		CellID:             cellID,
		RequiredComponents: requiredComponents,
	}); err != nil {
		return fmt.Errorf("ensure cell health: %w", err)
	}
	if _, err := store.EnsureDefaultWorkerGroup(ctx, cellID); err != nil {
		return fmt.Errorf("ensure default worker group: %w", err)
	}
	return nil
}
