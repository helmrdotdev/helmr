package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Enabled           bool
	RegionID          string
	RegionDisplayName string
	RegionLocation    string
	WorkerGroupName   string
	WorkerToken       string
}

func Apply(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if !cfg.Enabled {
		return nil
	}
	if pool == nil {
		return errors.New("bootstrap database is required")
	}
	cfg.RegionID = strings.TrimSpace(cfg.RegionID)
	cfg.RegionDisplayName = strings.TrimSpace(cfg.RegionDisplayName)
	cfg.RegionLocation = strings.TrimSpace(cfg.RegionLocation)
	cfg.WorkerGroupName = strings.TrimSpace(cfg.WorkerGroupName)
	if err := region.ValidateID(cfg.RegionID); err != nil {
		return fmt.Errorf("bootstrap region ID: %w", err)
	}
	if cfg.RegionDisplayName == "" {
		cfg.RegionDisplayName = cfg.RegionID
	}
	if err := workergroup.ValidateName(cfg.WorkerGroupName); err != nil {
		return fmt.Errorf("bootstrap worker group name: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bootstrap: %w", err)
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	if err := q.LockWorkerGroupCreationRegion(ctx, pglock.Key("helmr:worker-group-create:"+cfg.RegionID)); err != nil {
		return fmt.Errorf("lock bootstrap region: %w", err)
	}
	if _, err := q.GetRegion(ctx, cfg.RegionID); errors.Is(err, pgx.ErrNoRows) {
		if _, err := q.CreateRegion(ctx, db.CreateRegionParams{
			ID: cfg.RegionID, DisplayName: cfg.RegionDisplayName, Location: cfg.RegionLocation,
		}); err != nil {
			return fmt.Errorf("create bootstrap region: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get bootstrap region: %w", err)
	}
	if _, err := q.GetWorkerGroupByRegionName(ctx, db.GetWorkerGroupByRegionNameParams{
		RegionID: cfg.RegionID, Name: cfg.WorkerGroupName,
	}); err == nil {
		return commit(ctx, tx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get bootstrap worker group: %w", err)
	}
	tokenHash, err := workergroup.ParseEnrollmentToken(cfg.WorkerToken)
	if err != nil {
		return err
	}
	if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID: uuid.Must(uuid.NewV7()).String(), TokenID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		TokenHash: tokenHash, RegionID: cfg.RegionID, Name: cfg.WorkerGroupName, Description: "",
		AllowsRun: true, AllowsBuild: true, RequiredCPUMillis: 1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredBuildCacheBytes: 0,
		RequiredArtifactCacheBytes: 0, RequiredVMSlots: 1,
		ObservationTtlSeconds: 120,
	}); err != nil {
		return fmt.Errorf("create bootstrap worker group: %w", err)
	}
	return commit(ctx, tx)
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit bootstrap: %w", err)
	}
	return nil
}
