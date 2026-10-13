package session

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// notReadyOnLock reports a missing row or a lock wait that timed out as a
// retryable outcome.
func notReadyOnLock(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || db.IsLockNotAvailable(err) {
		return ErrNotReady
	}
	return err
}

// shortLockTimeout bounds lock waits of host-facing transactions.
func shortLockTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
	return err
}
