package idempotency

import (
	"context"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

// CollectReceipts drops expired response bodies, never the operation identity.
// Each bounded batch skips busy claims so replay and concurrent collectors do
// not wait on one another.
func CollectReceipts(ctx context.Context, queries *db.Queries, log *slog.Logger) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := queries.PruneExpiredIdempotencyReceipts(batchCtx, 1000)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "collect expired operation receipts", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
