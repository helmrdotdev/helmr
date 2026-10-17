// Package dbnotify delivers transaction-commit hints. Durable database state,
// never a notification, remains the authority for every consumer.
package dbnotify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hub uses one dedicated PostgreSQL connection per process, regardless of the
// number of connected browsers or workers. Slow subscribers coalesce hints.
type Hub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

func New() *Hub { return &Hub{subscribers: make(map[string]map[chan struct{}]struct{})} }

// Subscribe must precede reading durable state, including after reconnection.
func (h *Hub) Subscribe(topic string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subscribers[topic] == nil {
		h.subscribers[topic] = make(map[chan struct{}]struct{})
	}
	h.subscribers[topic][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subscribers[topic], ch)
		if len(h.subscribers[topic]) == 0 {
			delete(h.subscribers, topic)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) publish(topic string, all bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, subscribers := range h.subscribers {
		if !all && key != topic {
			continue
		}
		for ch := range subscribers {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run reconnects and wakes every subscriber after LISTEN is committed. This
// closes the listen/snapshot race and catches changes committed while offline.
func (h *Hub) Run(ctx context.Context, config *pgx.ConnConfig, log *slog.Logger) error {
	for {
		err := h.listen(ctx, config)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.WarnContext(ctx, "database notification connection lost", "error", err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (h *Hub) listen(ctx context.Context, config *pgx.ConnConfig) error {
	conn, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN helmr_changes"); err != nil {
		return err
	}
	h.publish("", true)
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.publish(notification.Payload, false)
	}
}
