package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

// CollectComputerCommandResults prunes expired, reconciled command payloads.
// Execution identity and outcome facts remain durable.
func CollectComputerCommandResults(ctx context.Context, queries *db.Queries, log *slog.Logger) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := queries.PruneExpiredComputerCommandResults(batchCtx, 1000)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "collect expired Command results", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
