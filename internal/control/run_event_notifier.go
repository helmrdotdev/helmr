package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runEventsNotifyChannel = "helmr_run_events"

type runEventSubscriptionNotifier interface {
	SubscribeRunEvents(ctx context.Context, runID pgtype.UUID) (<-chan struct{}, func())
}

type PostgresRunEventNotifier struct {
	log *slog.Logger

	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

type runEventNotificationPayload struct {
	RunID string `json:"run_id"`
}

func NewPostgresRunEventNotifier(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*PostgresRunEventNotifier, error) {
	notifier := &PostgresRunEventNotifier{
		log:         log,
		subscribers: map[string]map[chan struct{}]struct{}{},
	}
	if notifier.log == nil {
		notifier.log = slog.Default()
	}
	go notifier.run(ctx, pool)
	return notifier, nil
}

func (b *PostgresRunEventNotifier) run(ctx context.Context, pool *pgxpool.Pool) {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := b.listenOnce(ctx, pool)
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("run event notifier listener reconnecting", "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (b *PostgresRunEventNotifier) listenOnce(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+runEventsNotifyChannel); err != nil {
		return err
	}
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification.Channel != runEventsNotifyChannel {
			continue
		}
		var payload runEventNotificationPayload
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil || payload.RunID == "" {
			b.log.Warn("invalid run event notification", "payload", notification.Payload, "error", err)
			continue
		}
		b.publish(payload.RunID)
	}
}

func (b *PostgresRunEventNotifier) SubscribeRunEvents(_ context.Context, runID pgtype.UUID) (<-chan struct{}, func()) {
	key := ids.MustFromPG(runID).String()
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subscribers[key] == nil {
		b.subscribers[key] = map[chan struct{}]struct{}{}
	}
	b.subscribers[key][ch] = struct{}{}
	b.mu.Unlock()
	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subscribers[key], ch)
		if len(b.subscribers[key]) == 0 {
			delete(b.subscribers, key)
		}
		close(ch)
	}
	return ch, unsubscribe
}

func (b *PostgresRunEventNotifier) publish(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
