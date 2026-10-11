package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// commandOperation selects the vocabulary of a Command operation's errors.
type commandOperation int

const (
	commandClaimOperation commandOperation = iota + 1
	commandCompletionOperation
	commandLogAppendOperation
	commandCreateOperation
	commandGetOperation
	commandCancelOperation
)

// commandChanged is the conflict each worker operation reports when the
// Command authority it fences changed.
var commandChanged = map[commandOperation]string{
	commandClaimOperation:      "command claim is stale",
	commandCompletionOperation: "command completion is stale or differs from its receipt",
	commandLogAppendOperation:  "command log producer is stale or sequence contains different content",
}

// commandNotFound is the message each public read reports for a Command
// outside the caller's scope.
var commandNotFound = map[commandOperation]string{
	commandGetOperation:    "computer exec process was not found",
	commandCancelOperation: "Command was not found",
}

// commandFailed is the log message of a public operation's unavailable
// errors.
var commandFailed = map[commandOperation]string{
	commandCreateOperation: "execute Computer failed",
	commandGetOperation:    "read Computer command failed",
	commandCancelOperation: "cancel Computer command failed",
}

// commandError maps a command owner error to the API error the caller is
// told about. Stale credential claims ask a worker to re-authenticate before
// anything else is considered. An error a worker operation does not describe
// is an internal error, except that a failed log append stays retryable; an
// error a public operation does not describe reports the Computer authority
// as unavailable and retryable.
func commandError(err error, operation commandOperation) error {
	switch operation {
	case commandCreateOperation, commandGetOperation, commandCancelOperation:
		return publicCommandError(err, operation)
	}
	switch {
	case errors.Is(err, workergroup.ErrStaleClaims):
		return unauthorized(errors.New("worker authentication is required"))
	case errors.Is(err, secret.ErrDeliveryUnavailable):
		return conflict(codedError{code: "secret_unavailable", message: "command Secret delivery is unavailable"})
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

func publicCommandError(err error, operation commandOperation) error {
	var classified apiError
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	var input command.InputError
	switch {
	case errors.As(err, &classified):
		return classified
	case errors.As(err, &expired):
		return gone(expired)
	case errors.As(err, &idempotencyConflict):
		return conflict(codedError{code: "idempotency_conflict", message: err.Error()})
	case errors.Is(err, command.ErrNotFound) && commandNotFound[operation] != "":
		return notFound(codedError{code: "computer_command_not_found", message: commandNotFound[operation]})
	}
	if operation == commandCreateOperation {
		switch {
		case errors.As(err, &input) && input.Kind == command.InputStdinTooLarge:
			return tooLarge(codedError{code: "computer_stdin_too_large", message: err.Error()})
		case errors.As(err, &input) && input.Kind == command.InputTooLarge:
			return tooLarge(codedError{code: "computer_command_request_too_large", message: err.Error()})
		case errors.As(err, &input):
			return badRequest(codedError{code: "invalid_computer_command", message: err.Error()})
		case errors.Is(err, computer.ErrSecretUnavailable):
			return conflict(codedError{code: "secret_unavailable", message: err.Error()})
		case errors.Is(err, computer.ErrNotFound):
			return notFound(codedError{code: "computer_not_found", message: err.Error()})
		case errors.Is(err, computer.ErrBusy):
			return conflict(codedError{code: "computer_busy", message: err.Error(), retryable: true})
		case errors.Is(err, computer.ErrRecoveryRequired):
			return conflict(codedError{code: "computer_recovery_required", message: "computer requires recovery"})
		case errors.Is(err, computer.ErrDeleting):
			return conflict(codedError{code: "computer_deleting", message: "computer is deleting"})
		case errors.Is(err, computer.ErrPreparationExhausted):
			return conflict(codedError{code: "computer_preparation_exhausted", message: "Computer preparation limit reached"})
		}
	}
	return unavailable(codedError{code: "computer_authority_unavailable", message: errComputerAuthorityUnavailable.Error(), retryable: true})
}

// writeCommandError writes the API error of a public Command operation and
// logs the errors it reports as unavailable.
func (s *Server) writeCommandError(w http.ResponseWriter, err error, operation commandOperation) {
	mapped := commandError(err, operation)
	if errorStatus(mapped) == http.StatusServiceUnavailable {
		s.log.Error(commandFailed[operation], "error", err)
	}
	writeError(w, mapped)
}
