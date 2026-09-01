package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const (
	DeliveredRetention         = 24 * time.Hour
	lifecycleBatchSize         = int32(2500)
	lifecyclePruneBatchCount   = 16
	deadLetterObservationLimit = int64(1000)
	lifecycleEvery             = time.Minute
)

var supportedTopics = []string{
	"session.input.reconcile",
	"session.close.reconcile",
	"token.reconcile",
	"secret.revoked",
}

type LifecycleStore interface {
	DeadLetterUnsupportedControlOutbox(context.Context, db.DeadLetterUnsupportedControlOutboxParams) ([]db.ControlOutbox, error)
	PruneDeliveredControlOutbox(context.Context, db.PruneDeliveredControlOutboxParams) (int64, error)
	ControlOutboxLifecycle(context.Context, int64) (db.ControlOutboxLifecycleRow, error)
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
		limit:    lifecycleBatchSize,
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
	deadLettered, err := l.store.DeadLetterUnsupportedControlOutbox(ctx, db.DeadLetterUnsupportedControlOutboxParams{
		SupportedTopics: supportedTopics,
		RowLimit:        l.limit,
	})
	if err != nil {
		return err
	}
	for _, message := range deadLettered {
		LogDeadLettered(l.log, pgvalue.MustUUIDValue(message.ID).String(), message.Topic, errors.New("unsupported control outbox topic"))
	}
	var prunedCount int64
	for range lifecyclePruneBatchCount {
		pruned, err := l.store.PruneDeliveredControlOutbox(ctx, db.PruneDeliveredControlOutboxParams{
			RetainFor: pgvalue.Interval(l.retain),
			RowLimit:  l.limit,
		})
		if err != nil {
			return err
		}
		prunedCount += pruned
		if pruned < int64(l.limit) {
			break
		}
	}
	stats, err := l.store.ControlOutboxLifecycle(ctx, deadLetterObservationLimit)
	if err != nil {
		return err
	}
	attrs := []any{
		"pruned", prunedCount,
		"dead_lettered_rows", stats.DeadLetteredRows,
		"dead_lettered_overflow", stats.DeadLetteredOverflow,
	}
	if prunedCount == 0 && stats.DeadLetteredRows == 0 && !stats.OldestEligibleAt.Valid {
		return nil
	}
	if stats.OldestEligibleAt.Valid {
		attrs = append(attrs, "oldest_eligible_age", l.now().UTC().Sub(stats.OldestEligibleAt.Time))
	}
	l.log.Info("control outbox lifecycle", attrs...)
	return nil
}
