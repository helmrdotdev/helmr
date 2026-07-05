package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultRoutingFreshness = 120 * time.Second

type BootstrapStore interface {
	EnsureRegion(context.Context, db.EnsureRegionParams) (db.Region, error)
	GetRegion(context.Context, string) (db.Region, error)
	EnsureDefaultWorkerGroup(context.Context, db.EnsureDefaultWorkerGroupParams) (db.WorkerGroup, error)
	ReportWorkerGroupHealth(context.Context, db.ReportWorkerGroupHealthParams) (db.WorkerGroup, error)
}

type BootstrapConfig struct {
	RegionID           string
	DefaultRegionID    string
	Provider           string
	ProviderRegion     string
	RegionDisplayName  string
	WorkerGroupID      string
	RequiredComponents []string
}

func Bootstrap(ctx context.Context, store BootstrapStore, cfg BootstrapConfig) error {
	if store == nil {
		return errors.New("worker group bootstrap store is required")
	}
	regionID := strings.TrimSpace(cfg.RegionID)
	provider := strings.TrimSpace(cfg.Provider)
	providerRegion := strings.TrimSpace(cfg.ProviderRegion)
	displayName := strings.TrimSpace(cfg.RegionDisplayName)
	workerGroupID := strings.TrimSpace(cfg.WorkerGroupID)
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
	if workerGroupID == "" {
		return errors.New("worker group id is required")
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
	if _, err := store.EnsureDefaultWorkerGroup(ctx, db.EnsureDefaultWorkerGroupParams{
		ID:       workerGroupID,
		RegionID: regionID,
	}); err != nil {
		return fmt.Errorf("ensure default worker group: %w", err)
	}
	if _, err := store.ReportWorkerGroupHealth(ctx, db.ReportWorkerGroupHealthParams{
		HealthState:   db.WorkerGroupHealthStateHealthy,
		FreshFor:      pgtype.Interval{Microseconds: defaultRoutingFreshness.Microseconds(), Valid: true},
		HealthDetails: []byte(`{}`),
		WorkerGroupID: workerGroupID,
	}); err != nil {
		return fmt.Errorf("ensure worker group health: %w", err)
	}
	return nil
}
