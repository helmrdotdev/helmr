package computer

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Operation outcomes. The Session and dispatch owners return them unchanged
// and declare their own outcomes as these values, so a Computer outcome keeps
// its meaning through every caller.
var (
	ErrInvalidInput = errors.New("invalid operation input")
	ErrDenied       = errors.New("operation not authorized")
	ErrConflict     = errors.New("request identity conflicts with its original request")
	ErrNotReady     = errors.New("execution is not ready")
	ErrTerminal     = errors.New("turn is terminal")
)

// hideMissing reports a row the caller cannot see as a denial, so absence
// and lack of authority are indistinguishable.
func hideMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

// notReadyOnLock reports a missing row or a lock wait that timed out as a
// retryable outcome.
func notReadyOnLock(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || db.IsLockNotAvailable(err) {
		return ErrNotReady
	}
	return err
}

// shortLockTimeout bounds lock waits of allocation-path transactions.
func shortLockTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
	return err
}

// LockHost takes the worker supply gate (step 2 of the global order) for a
// host-authenticated caller and denies a host that is no longer eligible.
func LockHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) error {
	eligible, err := workergroup.LockRuntimeHost(ctx, tx, host)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrDenied
	}
	return nil
}
