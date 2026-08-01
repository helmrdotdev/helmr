package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const (
	imageCacheRetirementInterval = time.Hour
	imageCacheRetirementLimit    = int32(100)
)

type imageCacheRetirementStore interface {
	ListDueEnvironmentImageCacheRetirements(context.Context, int32) ([]db.ListDueEnvironmentImageCacheRetirementsRow, error)
	MarkEnvironmentImageCacheRetired(context.Context, db.MarkEnvironmentImageCacheRetiredParams) (int64, error)
}

type imageCacheRetirementWorkflow struct {
	log     *slog.Logger
	store   imageCacheRetirementStore
	retirer imagecache.RepositoryRetirer
}

// RunImageCacheRetirement starts the provider-neutral best-effort retirement
// loop. Provider or database failures are logged and retried; they never gate
// Control admission.
func RunImageCacheRetirement(
	ctx context.Context,
	log *slog.Logger,
	store imageCacheRetirementStore,
	retirer imagecache.RepositoryRetirer,
) {
	workflow := imageCacheRetirementWorkflow{log: log, store: store, retirer: retirer}
	ticker := time.NewTicker(imageCacheRetirementInterval)
	defer ticker.Stop()
	for {
		if err := workflow.reconcile(ctx, imageCacheRetirementLimit); err != nil && ctx.Err() == nil {
			if log == nil {
				log = slog.Default()
			}
			log.Error("retire Workspace image cache repositories", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (workflow imageCacheRetirementWorkflow) reconcile(ctx context.Context, limit int32) error {
	if workflow.store == nil {
		return errors.New("image cache retirement storage is required")
	}
	if workflow.retirer == nil {
		return errors.New("image cache repository retirer is required")
	}
	candidates, err := workflow.store.ListDueEnvironmentImageCacheRetirements(ctx, limit)
	if err != nil {
		return fmt.Errorf("list due Environment cache retirements: %w", err)
	}
	var failures []error
	for _, candidate := range candidates {
		jobID, jobErr := pgvalue.UUIDValue(candidate.ID)
		environmentID, environmentErr := pgvalue.UUIDValue(candidate.TargetID)
		if jobErr != nil || environmentErr != nil {
			failures = append(failures, fmt.Errorf("invalid Environment cache retirement identity: %w", errors.Join(jobErr, environmentErr)))
			continue
		}
		if err := workflow.retire(ctx, jobID, environmentID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (workflow imageCacheRetirementWorkflow) retire(ctx context.Context, jobID, environmentID uuid.UUID) error {
	if err := workflow.retirer.Retire(ctx, environmentID); err != nil {
		return fmt.Errorf("retire Environment %s image cache repository: %w", environmentID, err)
	}
	if _, err := workflow.store.MarkEnvironmentImageCacheRetired(ctx, db.MarkEnvironmentImageCacheRetiredParams{
		ID:            pgvalue.UUID(jobID),
		EnvironmentID: pgvalue.UUID(environmentID),
	}); err != nil {
		return fmt.Errorf("mark Environment %s image cache repository retired: %w", environmentID, err)
	}
	return nil
}
