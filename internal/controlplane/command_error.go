package controlplane

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// commandOperation selects the vocabulary of a Command operation's errors.
type commandOperation int

const (
	commandClaimOperation commandOperation = iota + 1
	commandCompletionOperation
	commandLogAppendOperation
)

// commandChanged is the conflict each worker operation reports when the
// Command authority it fences changed.
var commandChanged = map[commandOperation]string{
	commandClaimOperation:      "command claim is stale",
	commandCompletionOperation: "command completion is stale or differs from its receipt",
	commandLogAppendOperation:  "command log producer is stale or sequence contains different content",
}

// commandError maps a command owner error to the API error the worker is
// told about. Stale credential claims ask the worker to re-authenticate
// before anything else is considered. An error the operation does not
// describe is an internal error, except that a failed log append stays
// retryable.
func commandError(err error, operation commandOperation) error {
	switch {
	case errors.Is(err, workergroup.ErrStaleClaims):
		return unauthorized(errors.New("worker authentication is required"))
	case errors.Is(err, command.ErrChanged):
		return conflict(errors.New(commandChanged[operation]))
	case operation == commandCompletionOperation && errors.Is(err, command.ErrInvalidCompletion):
		return badRequest(err)
	case operation == commandLogAppendOperation && errors.Is(err, command.ErrInvalidLog):
		return badRequest(command.ErrInvalidLog)
	}
	switch operation {
	case commandClaimOperation:
		return errors.New("claim computer command")
	case commandCompletionOperation:
		return errors.New("complete computer command")
	default:
		return unavailable(errors.New("append command log failed"))
	}
}
