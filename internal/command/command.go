// Package command owns Computer Commands: their creation with its
// idempotency receipt, scoped reads and public cancellation; the durable
// transitions a worker host reports for the processes it runs (claim,
// cancellation and release, completion, reconciliation and log output); the
// dispatcher's recovery of Commands whose physical authority was lost; the
// failure of pending Commands and the retention of their results. Worker and
// recovery operations lock, in the global order, any Secrets they deliver or
// validate, then the worker supply they compare claims against, then the
// Computer and the Command's Instance through the computer owner, then the
// Command. Creation acquires its idempotency claim, then locks the Secrets
// and the Computer; public cancellation acquires its claim and updates the
// Command without Computer or Instance locks.
package command

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrChanged reports that the Command, its Instance, its Computer or the
// worker epoch no longer holds the authority the operation requires, or
// that a reported result or log record differs from the one already
// recorded. Stale worker credential claims are reported as
// workergroup.ErrStaleClaims instead.
var ErrChanged = errors.New("command authority changed")

// ErrInvalidCompletion reports a completion report that does not describe
// one unambiguous Command result.
var ErrInvalidCompletion = errors.New("invalid command completion")

// ErrInvalidLog reports a log record whose identity, content or observation
// is invalid.
var ErrInvalidLog = errors.New("invalid command log identity, content or observation")

// changed reports a fence that no longer holds, which the internal
// transaction signals with pgx.ErrNoRows, as ErrChanged.
func changed(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChanged
	}
	return err
}
