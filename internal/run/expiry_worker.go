package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const (
	queuedChildExpiryInterval = 250 * time.Millisecond
	queuedChildExpiryLimit    = int32(100)
)

type queuedChildExpiryDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type QueuedChildExpiryWorker struct {
	log      *slog.Logger
	db       queuedChildExpiryDB
	interval time.Duration
	limit    int32
}

func NewQueuedChildExpiryWorker(log *slog.Logger, database queuedChildExpiryDB) (*QueuedChildExpiryWorker, error) {
	if database == nil {
		return nil, errors.New("queued child expiry database is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &QueuedChildExpiryWorker{
		log: log, db: database,
		interval: queuedChildExpiryInterval, limit: queuedChildExpiryLimit,
	}, nil
}

func (w *QueuedChildExpiryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.expire(ctx, w.limit); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("expire queued child Runs failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *QueuedChildExpiryWorker) expire(ctx context.Context, limit int32) error {
	candidates, err := db.New(w.db).ListExpiredParentOwnedChildRuns(ctx, limit)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := w.expireOne(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (w *QueuedChildExpiryWorker) expireOne(
	ctx context.Context,
	candidate db.ListExpiredParentOwnedChildRunsRow,
) (err error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin queued child expiry: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback queued child expiry: %w", rollbackErr))
		}
	}()
	_, err = ExpireParentOwnedChild(ctx, tx, ChildExpiryRequest{
		OrgID:         pgvalue.MustUUIDValue(candidate.OrgID),
		ProjectID:     pgvalue.MustUUIDValue(candidate.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(candidate.EnvironmentID),
		ParentRunID:   pgvalue.MustUUIDValue(candidate.ParentRunID),
		ChildRunID:    pgvalue.MustUUIDValue(candidate.ID),
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit queued child expiry: %w", err)
	}
	committed = true
	return nil
}
