package run

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	retryReadyInterval = 250 * time.Millisecond
	retryReadyLimit    = int32(100)
)

type RetryReadyStore interface {
	ReadyRunRetries(context.Context, int32) ([]db.ReadyRunRetriesRow, error)
}

type RetryReadyWorker struct {
	log      *slog.Logger
	store    RetryReadyStore
	interval time.Duration
	limit    int32
}

func NewRetryReadyWorker(log *slog.Logger, store RetryReadyStore) (*RetryReadyWorker, error) {
	if store == nil {
		return nil, errors.New("run retry readiness store is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RetryReadyWorker{
		log: log, store: store,
		interval: retryReadyInterval, limit: retryReadyLimit,
	}, nil
}

func (w *RetryReadyWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.ready(ctx, w.limit); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("ready delayed Run retries failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *RetryReadyWorker) ready(ctx context.Context, limit int32) error {
	_, err := w.store.ReadyRunRetries(ctx, limit)
	return err
}
