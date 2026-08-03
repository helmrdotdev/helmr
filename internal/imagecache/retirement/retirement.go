package retirement

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
	pollInterval = time.Hour
	batchLimit   = int32(100)
)

type Store interface {
	ListDueEnvironmentImageCacheRetirements(context.Context, int32) ([]db.ListDueEnvironmentImageCacheRetirementsRow, error)
	MarkEnvironmentImageCacheRetired(context.Context, db.MarkEnvironmentImageCacheRetiredParams) (int64, error)
}

type Worker struct {
	log      *slog.Logger
	store    Store
	retirer  imagecache.RepositoryRetirer
	interval time.Duration
	limit    int32
}

func NewWorker(log *slog.Logger, store Store, retirer imagecache.RepositoryRetirer) (*Worker, error) {
	if store == nil {
		return nil, errors.New("image cache retirement store is required")
	}
	if retirer == nil {
		return nil, errors.New("image cache repository retirer is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		log: log, store: store, retirer: retirer,
		interval: pollInterval, limit: batchLimit,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.reconcile(ctx, w.limit); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("retire Workspace image cache repositories", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) reconcile(ctx context.Context, limit int32) error {
	candidates, err := w.store.ListDueEnvironmentImageCacheRetirements(ctx, limit)
	if err != nil {
		return fmt.Errorf("list due environment cache retirements: %w", err)
	}
	var failures []error
	for _, candidate := range candidates {
		jobID, jobErr := pgvalue.UUIDValue(candidate.ID)
		environmentID, environmentErr := pgvalue.UUIDValue(candidate.TargetID)
		if jobErr != nil || environmentErr != nil {
			failures = append(failures, fmt.Errorf("invalid environment cache retirement identity: %w", errors.Join(jobErr, environmentErr)))
			continue
		}
		if err := w.retire(ctx, jobID, environmentID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (w *Worker) retire(ctx context.Context, jobID, environmentID uuid.UUID) error {
	if err := w.retirer.Retire(ctx, environmentID); err != nil {
		return fmt.Errorf("retire environment %s image cache repository: %w", environmentID, err)
	}
	if _, err := w.store.MarkEnvironmentImageCacheRetired(ctx, db.MarkEnvironmentImageCacheRetiredParams{
		ID:            pgvalue.UUID(jobID),
		EnvironmentID: pgvalue.UUID(environmentID),
	}); err != nil {
		return fmt.Errorf("mark environment %s image cache repository retired: %w", environmentID, err)
	}
	return nil
}
