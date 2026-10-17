package dbnotify

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Notify queues hints in the caller's data-changing transaction. PostgreSQL
// delivers them only on commit and coalesces identical topics in that transaction.
// Operation owners choose topics; notification delivery never replaces durable state.
func Notify(ctx context.Context, tx pgx.Tx, topics ...string) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify('helmr_changes', topic) FROM unnest($1::text[]) AS topic`, topics)
	return err
}
