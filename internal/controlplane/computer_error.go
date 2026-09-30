package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var errComputerAuthorityUnavailable = errors.New("computer authority is unavailable")

// computerOperation selects the input vocabulary of a Computer operation's
// errors: a rejected creation and a rejected reference carry different codes.
type computerOperation int

const (
	computerCreateOperation computerOperation = iota + 1
	computerDeleteOperation
	computerReadOperation
)

// computerError maps a Computer owner error to the API error the client is
// told about. Errors it does not describe are returned unchanged.
func computerError(err error, operation computerOperation) error {
	var input computer.InputError
	var keyConflict computer.KeyConflictError
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.As(err, &expired):
		return gone(expired)
	case errors.As(err, &input):
		code := "invalid_computer_reference"
		if operation == computerCreateOperation {
			code = "invalid_computer_create"
		}
		return badRequest(codedError{code: code, message: err.Error()})
	case errors.Is(err, computer.ErrNotFound):
		return notFound(codedError{code: "computer_not_found", message: err.Error()})
	case errors.Is(err, computer.ErrNotDeployed):
		return notFound(codedError{code: "computer_not_deployed", message: err.Error()})
	case errors.Is(err, computer.ErrBusy):
		return conflict(codedError{code: "computer_busy", message: err.Error(), retryable: true})
	case errors.Is(err, computer.ErrSecretUnavailable):
		return conflict(codedError{code: "secret_unavailable", message: err.Error()})
	case errors.As(err, &keyConflict):
		return conflict(codedError{code: "computer_key_conflict", message: err.Error()})
	case errors.As(err, &idempotencyConflict):
		return conflict(codedError{code: "idempotency_conflict", message: err.Error()})
	default:
		return err
	}
}

// writeComputerError writes a public Computer operation's failure. Failures
// the client is not told about are logged and reported as retryable
// unavailability of Computer authority.
func (s *Server) writeComputerError(w http.ResponseWriter, err error, operation computerOperation, logMessage string) {
	mapped := computerError(err, operation)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error(logMessage, "error", err)
	writeError(w, unavailable(codedError{
		code:      "computer_authority_unavailable",
		message:   errComputerAuthorityUnavailable.Error(),
		retryable: true,
	}))
}

// workerComputerFailure describes a run-sourced Computer operation's failure
// as the runtime operation failure its response carries.
func workerComputerFailure(err error, operation computerOperation) (workerapi.RuntimeOperationFailure, bool) {
	mapped := computerError(err, operation)
	if errorStatus(mapped) == http.StatusInternalServerError {
		return workerapi.RuntimeOperationFailure{}, false
	}
	failure := workerapi.RuntimeOperationFailure{Message: mapped.Error()}
	var coder errorCoder
	if errors.As(mapped, &coder) {
		failure.Code = coder.ErrorCode()
	}
	var retryer errorRetryer
	if errors.As(mapped, &retryer) {
		failure.Retryable = retryer.ErrorRetryable()
	}
	return failure, true
}
