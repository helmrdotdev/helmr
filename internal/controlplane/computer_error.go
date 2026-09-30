package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

var errComputerAuthorityUnavailable = errors.New("computer authority is unavailable")

// computerOperation selects the vocabulary of a Computer operation's errors:
// a rejected creation and a rejected reference carry different codes, and
// each Instance operation names the authority that changed.
type computerOperation int

const (
	computerCreateOperation computerOperation = iota + 1
	computerDeleteOperation
	computerReadOperation
	computerInstanceObservationOperation
	computerInstanceClaimOperation
	computerInstanceRenewalOperation
	computerRunCleanupOperation
	computerRestorePlanOperation
	computerKeyDeliveryOperation
	computerInitialObjectOperation
	computerInitialVersionOperation
	computerCheckpointObjectOperation
	computerCheckpointReadyOperation
	computerSaveOperation
)

// instance reports whether the operation is authenticated by a worker host
// principal on an Instance. Run-sourced aggregate operations report stale
// claims through their Run source instead.
func (o computerOperation) instance() bool {
	return o >= computerInstanceObservationOperation
}

// publication reports whether the operation records disk objects or
// publishes a checkpoint or save, whose deterministic admission failures are
// changed authority as well.
func (o computerOperation) publication() bool {
	return o == computerCheckpointObjectOperation || o == computerCheckpointReadyOperation || o == computerSaveOperation
}

// computerAuthorityChanged is the conflict each Instance operation reports
// when the authority it fences changed.
var computerAuthorityChanged = map[computerOperation]string{
	computerInstanceObservationOperation: "runtime instance fence is stale",
	computerInstanceRenewalOperation:     "Computer Instance writer is stale",
	computerRunCleanupOperation:          "Run cleanup authority is stale",
	computerRestorePlanOperation:         "Computer restore authority changed",
	computerCheckpointObjectOperation:    "computer publication authority changed",
	computerCheckpointReadyOperation:     "computer publication authority changed",
	computerSaveOperation:                "computer publication authority changed",
}

// computerError maps a Computer owner error to the API error the client is
// told about. Errors it does not describe are returned unchanged.
func computerError(err error, operation computerOperation) error {
	var input computer.InputError
	var keyConflict computer.KeyConflictError
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	var objectConflict computer.ObjectConflictError
	if operation.instance() && errors.Is(err, workergroup.ErrStaleClaims) {
		return unauthorized(errors.New("worker authentication is required"))
	}
	switch operation {
	case computerKeyDeliveryOperation:
		// Only a rejected authority is a conflict; any other failure leaves
		// the delivery retryable.
		if errors.Is(err, computer.ErrKeyUnavailable) {
			return conflict(computer.ErrKeyUnavailable)
		}
		return unavailable(errors.New("computer key delivery is unavailable"))
	case computerInitialObjectOperation:
		switch {
		case errors.Is(err, computer.ErrStorageUnavailable):
			return unavailable(computer.ErrStorageUnavailable)
		case errors.As(err, &objectConflict):
			return conflict(objectConflict)
		default:
			return conflict(errors.New("computer object authority or registration changed"))
		}
	case computerInitialVersionOperation:
		return conflict(errors.New("computer generation publication is unavailable"))
	}
	switch {
	case errors.Is(err, computer.ErrAuthorityChanged) && computerAuthorityChanged[operation] != "",
		operation.publication() && isDeterministicWorkerAdmission(err):
		return conflict(errors.New(computerAuthorityChanged[operation]))
	case operation.publication() && errors.As(err, &objectConflict):
		return conflict(objectConflict)
	case operation == computerCheckpointObjectOperation && errors.Is(err, computer.ErrStorageUnavailable):
		return unavailable(errors.New("computer object unavailable"))
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

// writeComputerPublicationError writes the failure of a worker's disk object
// or publication operation. Failures the worker is not told about are logged
// and reported as internal.
func (s *Server) writeComputerPublicationError(w http.ResponseWriter, err error, operation computerOperation) {
	mapped := computerError(err, operation)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("Computer object publication failed", "error", err)
	writeError(w, errors.New("computer object publication failed"))
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
