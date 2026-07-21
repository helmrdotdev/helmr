package control

import (
	"context"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	runRetryReadyInterval = 250 * time.Millisecond
	runRetryReadyLimit    = int32(100)
)

type runRetryReadyStore interface {
	ReadyRunRetries(context.Context, int32) ([]db.ReadyRunRetriesRow, error)
}

type runRetryReadyWorkflow struct {
	log   *slog.Logger
	store runRetryReadyStore
}

func (w runRetryReadyWorkflow) run(ctx context.Context) {
	ticker := time.NewTicker(runRetryReadyInterval)
	defer ticker.Stop()
	for {
		if err := w.ready(ctx, runRetryReadyLimit); err != nil && ctx.Err() == nil && w.log != nil {
			w.log.Error("ready delayed Run retries failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w runRetryReadyWorkflow) ready(ctx context.Context, limit int32) error {
	_, err := w.store.ReadyRunRetries(ctx, limit)
	return err
}
