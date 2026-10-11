// Package command owns ordinary Computer process admission, exact physical-lease
// execution, cancellation, output and recovery. Worker operations lock supply,
// immutable Secret bindings, the Computer, its exact lease and then the Command.
// Recovery needs Secret and owner locks but grants no execution. Public creation
// and cancellation bind platform retry identity in the mutation transaction.
package command

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrChanged reports that the Command, its lease, its Computer or the
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
