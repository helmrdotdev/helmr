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
	computerCheckpointRegisterOperation
	computerCheckpointObjectOperation
	computerCheckpointReadyOperation
	computerCheckpointFailedOperation
	computerSaveOperation
)

// instance reports whether the operation is authenticated by a worker host
// principal on an Instance. Run-sourced aggregate operations report stale
// claims through their Run source instead.
func (o computerOperation) instance() bool {
	return o >= computerInstanceObservationOperation
}

// publication reports whether the operation registers a checkpoint, records
// disk objects or publishes a checkpoint or save, whose deterministic
// admission failures are changed authority as well.
func (o computerOperation) publication() bool {
	return o == computerCheckpointRegisterOperation || o == computerCheckpointObjectOperation || o == computerCheckpointReadyOperation || o == computerSaveOperation
}

// recordsObjects reports whether the operation records disk objects or
// publishes a version from them, whose object conflicts it reports.
func (o computerOperation) recordsObjects() bool {
	return o.publication() || o == computerInitialObjectOperation || o == computerInitialVersionOperation
}

// computerAuthorityChanged is the conflict each Instance operation reports
// when the authority it fences changed.
var computerAuthorityChanged = map[computerOperation]string{
	computerInstanceObservationOperation: "runtime instance fence is stale",
	computerInstanceRenewalOperation:     "Computer Instance writer is stale",
	computerRunCleanupOperation:          "Run cleanup authority is stale",
	computerRestorePlanOperation:         "Computer restore authority changed",
	computerKeyDeliveryOperation:         "computer preparation authority changed",
	computerInitialObjectOperation:       "computer preparation authority changed",
	computerInitialVersionOperation:      "computer preparation authority changed",
	computerCheckpointRegisterOperation:  "checkpoint registration is stale or differs from its candidate",
	computerCheckpointObjectOperation:    "computer publication authority changed",
	computerCheckpointReadyOperation:     "checkpoint ready source or candidate changed",
	computerCheckpointFailedOperation:    "checkpoint failure is stale or differs from its committed receipt",
	computerSaveOperation:                "computer publication authority changed",
}

// computerError maps a Computer owner error to the API error the client is
// told about: stale claims first, then malformed input as 400, deterministic
// conflicts including changed authority as 409 and recognized dependency
// unavailability as 503. Errors it does not describe are returned unchanged
// for the caller to log and report as internal.
func computerError(err error, operation computerOperation) error {
	var input computer.InputError
	var keyConflict computer.KeyConflictError
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	var objectConflict computer.ObjectConflictError
	if operation.instance() && errors.Is(err, workergroup.ErrStaleClaims) {
		return unauthorized(errors.New("worker authentication is required"))
	}
	if errors.Is(err, computer.ErrCheckpointCandidate) {
		return badRequest(err)
	}
	switch {
	case errors.Is(err, computer.ErrAuthorityChanged) && computerAuthorityChanged[operation] != "",
		operation.publication() && isDeterministicWorkerAdmission(err):
		return conflict(errors.New(computerAuthorityChanged[operation]))
	case operation.recordsObjects() && errors.As(err, &objectConflict):
		return conflict(objectConflict)
	case (operation == computerInitialObjectOperation || operation == computerCheckpointObjectOperation) && errors.Is(err, computer.ErrStorageUnavailable):
		return unavailable(errors.New("computer object unavailable"))
	case operation == computerKeyDeliveryOperation && errors.Is(err, computer.ErrKeyUnavailable):
		return conflict(computer.ErrKeyUnavailable)
	case operation == computerKeyDeliveryOperation && errors.Is(err, computer.ErrKeyProviderUnavailable):
		return unavailable(computer.ErrKeyProviderUnavailable)
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

// writeWorkerComputerError writes the failure of a worker host's Computer
// operation. Failures the worker is not told about are logged once and
// reported as internal without their cause.
func (s *Server) writeWorkerComputerError(w http.ResponseWriter, err error, operation computerOperation, logMessage string) {
	mapped := computerError(err, operation)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error(logMessage, "error", err)
	writeError(w, errors.New(logMessage))
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
