package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runEventsNotifyChannel = "helmr_run_events"

type runEventSubscriptionNotifier interface {
	SubscribeRunEvents(ctx context.Context, runID pgtype.UUID) (<-chan struct{}, func())
}

type PostgresRunEventNotifier struct {
	log        *slog.Logger
	connConfig *pgx.ConnConfig

	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
	done        chan struct{}
}

type runEventNotificationPayload struct {
	RunID string `json:"run_id"`
}

func NewPostgresRunEventNotifier(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*PostgresRunEventNotifier, error) {
	if pool == nil {
		return nil, errors.New("run event notifier database pool is required")
	}
	notifier := &PostgresRunEventNotifier{
		log:         log,
		connConfig:  pool.Config().ConnConfig.Copy(),
		subscribers: map[string]map[chan struct{}]struct{}{},
		done:        make(chan struct{}),
	}
	if notifier.log == nil {
		notifier.log = slog.Default()
	}
	go notifier.run(ctx)
	return notifier, nil
}

func (b *PostgresRunEventNotifier) run(ctx context.Context) {
	defer close(b.done)
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		started := time.Now()
		err := b.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= time.Minute {
			backoff = time.Second
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

func (b *PostgresRunEventNotifier) Shutdown(ctx context.Context) error {
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *PostgresRunEventNotifier) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, b.connConfig)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close(context.Background())
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+runEventsNotifyChannel); err != nil {
		return err
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
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
	}
	return ch, unsubscribe
}

func (b *PostgresRunEventNotifier) publish(runID string) {
	b.mu.Lock()
	subscribers := make([]chan struct{}, 0, len(b.subscribers[runID]))
	for ch := range b.subscribers[runID] {
		subscribers = append(subscribers, ch)
	}
	b.mu.Unlock()
	for _, ch := range subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
