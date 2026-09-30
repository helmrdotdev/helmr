package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5"
)

// BootstrapConfig names the region and worker group a self-hosted control
// plane seeds, and the enrollment token the seeded group accepts.
type BootstrapConfig struct {
	RegionID          string
	RegionDisplayName string
	RegionLocation    string
	GroupName         string
	EnrollmentToken   string
}

// Bootstrap seeds the configured region and worker group in one transaction,
// serialized with other worker group creators in the region. Existing rows are
// kept unchanged, and the enrollment token is parsed only when the group is
// created.
func Bootstrap(ctx context.Context, txb db.TxBeginner, cfg BootstrapConfig) error {
	cfg.RegionID = strings.TrimSpace(cfg.RegionID)
	cfg.GroupName = strings.TrimSpace(cfg.GroupName)
	if err := region.ValidateID(cfg.RegionID); err != nil {
		return fmt.Errorf("bootstrap region ID: %w", err)
	}
	if err := ValidateName(cfg.GroupName); err != nil {
		return fmt.Errorf("bootstrap worker group name: %w", err)
	}
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockWorkerGroupCreationRegion(ctx, creationLockKey(cfg.RegionID)); err != nil {
			return fmt.Errorf("lock bootstrap region: %w", err)
		}
		if err := region.Ensure(ctx, q, region.Details{
			ID: cfg.RegionID, DisplayName: cfg.RegionDisplayName, Location: cfg.RegionLocation,
		}); err != nil {
			return fmt.Errorf("ensure bootstrap region: %w", err)
		}
		_, err := q.GetWorkerGroupByRegionName(ctx, db.GetWorkerGroupByRegionNameParams{
			RegionID: cfg.RegionID, Name: cfg.GroupName,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get bootstrap worker group: %w", err)
		}
		tokenHash, err := auth.ParseEnrollmentToken(cfg.EnrollmentToken)
		if err != nil {
			return err
		}
		if _, err := q.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
			ID: pgvalue.UUID(uuid.NewV7()), TokenID: pgvalue.UUID(uuid.NewV7()),
			TokenHash: tokenHash, RegionID: cfg.RegionID, Name: cfg.GroupName, Description: "",
		}); err != nil {
			return fmt.Errorf("create bootstrap worker group: %w", err)
		}
		return nil
	})
}
