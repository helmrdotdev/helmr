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
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Dispatch outcomes belong to this owner; lower-owner errors pass through unchanged.
var (
	ErrInvalidInput = errors.New("invalid operation input")
	ErrDenied       = errors.New("operation not authorized")
	ErrConflict     = errors.New("request identity conflicts with its original request")
	ErrNotReady     = errors.New("execution is not ready")
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
	if errors.Is(err, pgx.ErrNoRows) || db.IsLockNotAvailable(err) || errors.Is(err, workergroup.ErrCapacityUnavailable) || errors.Is(err, workergroup.ErrCapacityExhausted) || errors.Is(err, workergroup.ErrCapacityIncompatible) {
		return ErrNotReady
	}
	return err
}

// allocationLockTimeout bounds every lock wait of an allocation transaction.
func allocationLockTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
	return err
}
