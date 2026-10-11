package slack

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Discovery supplies hints only; each participant transaction rechecks the live
// installation, publication and ordered cursor before committing publication intents.
func ReconcileProjection(ctx context.Context, pool db.TxBeginner, config ProjectionConfig, limit int) (int, error) {
	if !config.valid() || limit < 1 || limit > 64 {
		return 0, errors.New("invalid Slack projection configuration")
	}
	var ids []uuid.UUID
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.id FROM slack_thread_sources p
 JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.projected_event_seq<s.next_event_seq-1 ORDER BY p.projected_event_seq,p.id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	progressed := 0
	var failures []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return progressed, errors.Join(append(failures, err)...)
		}
		advanced, err := projectParticipant(ctx, pool, id, config)
		if advanced {
			progressed++
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return progressed, errors.Join(failures...)
}

// RunProjection drains bounded batches without waiting for native HTTP delivery.
// Cancellation returns only after the current transaction has finished.
func RunProjection(ctx context.Context, pool db.TxBeginner, config ProjectionConfig, log *slog.Logger) error {
	if pool == nil || log == nil || !config.valid() {
		return errors.New("the Slack projection dependencies are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		progressed, err := ReconcileProjection(ctx, pool, config, 64)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Slack event projection failed", "error", err)
		}
		if err == nil && progressed > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
