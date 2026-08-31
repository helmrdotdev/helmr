package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DeliveredRetention = 24 * time.Hour
	pruneBatchSize     = int32(100)
	lifecycleEvery     = 5 * time.Second
)

type LifecycleStore interface {
	PruneDeliveredControlOutbox(context.Context, db.PruneDeliveredControlOutboxParams) ([]pgtype.UUID, error)
	ControlOutboxLifecycle(context.Context) (db.ControlOutboxLifecycleRow, error)
}

type Lifecycle struct {
	log      *slog.Logger
	store    LifecycleStore
	interval time.Duration
	retain   time.Duration
	limit    int32
	now      func() time.Time
}

func NewLifecycle(log *slog.Logger, store LifecycleStore) (*Lifecycle, error) {
	if store == nil {
		return nil, errors.New("control outbox lifecycle store is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Lifecycle{
		log:      log,
		store:    store,
		interval: lifecycleEvery,
		retain:   DeliveredRetention,
		limit:    pruneBatchSize,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

func (l *Lifecycle) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		if err := l.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			l.log.Error("control outbox lifecycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *Lifecycle) tick(ctx context.Context) error {
	pruned, err := l.store.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
		RetainFor: pgvalue.Interval(l.retain),
		RowLimit:  l.limit,
	})
	if err != nil {
		return err
	}
	stats, err := l.store.ControlOutboxLifecycle(ctx)
	if err != nil {
		return err
	}
	attrs := []any{
		"pruned", len(pruned),
		"dead_lettered", stats.DeadLettered,
	}
	if stats.OldestPendingAt.Valid {
		attrs = append(attrs, "oldest_pending_age", l.now().UTC().Sub(stats.OldestPendingAt.Time))
	}
	l.log.Info("control outbox lifecycle", attrs...)
	return nil
}
