package computer

import (
	"context"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RunExecutionControls bounds accepted requests even when their host disappears.
// Failure reports do not assert that an in-flight physical transition rolled back.
func RunExecutionControls(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ReconcileExecutionControls(ctx, database); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "Computer execution control reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ReconcileExecutionControls(ctx context.Context, database db.TxBeginner) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		// Only receipt state changes; ownership uses the same Computer lock as input,
		// capture and completion. Skip busy Computers and retry on the next pass.
		_, err := tx.Exec(ctx, `WITH failed AS (
   SELECT r.environment_id,r.id,CASE
    WHEN c.deleted_at IS NOT NULL THEN 'computer_deleted'
    WHEN c.integrity_fault_at IS NOT NULL OR c.preparation_failed_at IS NOT NULL THEN 'computer_unavailable'
    WHEN cp.status IN ('cancelled','lost') THEN 'checkpoint_unavailable'
    ELSE 'execution_deadline_exceeded' END AS code
   FROM computer_controls r JOIN computers c ON (c.environment_id,c.id)=(r.environment_id,r.computer_id)
   LEFT JOIN checkpoints cp ON (cp.environment_id,cp.id)=(r.environment_id,r.checkpoint_id)
   WHERE r.status='accepted' AND ((r.deadline_at<=clock_timestamp() AND NOT (r.kind='hibernate' AND COALESCE(cp.status IN ('capturing','sealed'),false))) OR c.deleted_at IS NOT NULL OR c.integrity_fault_at IS NOT NULL OR c.preparation_failed_at IS NOT NULL OR cp.status IN ('cancelled','lost'))
   ORDER BY r.deadline_at,r.id LIMIT 100 FOR NO KEY UPDATE OF c SKIP LOCKED
  ) UPDATE computer_controls r SET status='failed',error_code=f.code,settled_at=clock_timestamp()
  FROM failed f WHERE (r.environment_id,r.id)=(f.environment_id,f.id) AND r.status='accepted'`)
		return err
	})
}
