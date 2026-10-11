package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"net/http"
)

var errComputerAuthorityUnavailable = errors.New("computer authority is unavailable")

type computerOperation int

const (
	computerCreateOperation computerOperation = iota + 1
	computerDeleteOperation
	computerReadOperation
)

func computerError(err error, operation computerOperation) error {
	var input computer.InputError
	var keyConflict computer.KeyConflictError
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.As(err, &expired):
		return gone(expired)
	case errors.As(err, &input):
		switch operation {
		case computerCreateOperation:
			return badRequest(codedError{code: "invalid_computer_create", message: err.Error()})
		case computerDeleteOperation, computerReadOperation:
			return badRequest(codedError{code: "invalid_computer_reference", message: err.Error()})
		default:
			return badRequest(input)
		}
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
