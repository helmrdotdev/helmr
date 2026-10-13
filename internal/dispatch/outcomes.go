// Package dispatch places leases and image builds onto worker supply,
// delivers them to hosts, and owns their host-level physical lifecycle: stop
// reports, expiry, provider-confirmed absence and successor-epoch recovery.
// It calls the computer owner for row transitions and the Session owner for
// process loss and Turn interruption.
package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// Dispatch outcomes are the Session and Computer outcomes, so a result keeps
// its meaning through every caller.
var (
	ErrInvalidInput = session.ErrInvalidInput
	ErrDenied       = session.ErrDenied
	ErrConflict     = session.ErrConflict
	ErrNotReady     = session.ErrNotReady
)

// hideMissing reports a row the caller cannot see as a denial.
func hideMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

// allocationError reports a missing row or a lock wait that timed out as a
// retryable outcome.
func allocationError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || db.IsLockNotAvailable(err) {
		return ErrNotReady
	}
	return err
}

// allocationLockTimeout bounds every lock wait of an allocation transaction.
func allocationLockTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
	return err
}
