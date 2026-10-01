package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// sessionOperation selects the vocabulary of a Session operation's errors.
type sessionOperation int

const (
	sessionStartOperation sessionOperation = iota + 1
	sessionWorkerStartOperation
)

// sessionError maps a session owner error to the API error the caller is
// told about. A public Actor start the session owner could not admit for an
// undescribed reason is retryable unavailability. A run-sourced Actor start
// asks the worker to re-authenticate on stale credential claims and reports
// a stale source as a conflict; its other undescribed errors are internal.
func sessionError(err error, operation sessionOperation) error {
	switch operation {
	case sessionStartOperation:
		return actorStartError(err)
	case sessionWorkerStartOperation:
		switch {
		case errors.Is(err, workergroup.ErrStaleClaims):
			return unauthorized(errors.New("worker authentication is required"))
		case errors.Is(err, run.ErrStaleSource):
			return conflict(run.ErrStaleSource)
		}
		return errors.New("start run-sourced actor")
	}
	return errors.New("session operation failed")
}

func actorStartError(err error) error {
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	var keyConflict session.KeyConflictError
	switch {
	case errors.As(err, &expired):
		return gone(expired)
	case errors.Is(err, computer.ErrPreparationExhausted):
		return conflict(codedError{code: "computer_preparation_exhausted", message: "Computer preparation limit reached"})
	case errors.As(err, &idempotencyConflict):
		return conflict(codedError{
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with an earlier actor start",
		})
	case errors.As(err, &keyConflict):
		return conflict(codedError{code: "actor_key_conflict", message: keyConflict.Error()})
	case errors.Is(err, session.ErrActorNotDeployed):
		return notFound(codedError{code: "actor_not_deployed", message: session.ErrActorNotDeployed.Error()})
	case errors.Is(err, session.ErrStartComputerNotFound):
		return notFound(codedError{code: "computer_not_found", message: session.ErrStartComputerNotFound.Error()})
	case errors.Is(err, session.ErrStartComputerUnavailable):
		return conflict(codedError{
			code:      "computer_unavailable",
			message:   session.ErrStartComputerUnavailable.Error(),
			retryable: true,
		})
	case errors.Is(err, session.ErrStartSecretUnavailable):
		return conflict(codedError{code: "secret_unavailable", message: session.ErrStartSecretUnavailable.Error()})
	case errors.Is(err, session.ErrStartInvalid):
		return badRequest(codedError{code: "invalid_actor_start", message: err.Error()})
	case errors.Is(err, session.ErrStartAuthority):
		return unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   session.ErrStartAuthority.Error(),
			retryable: true,
		})
	default:
		return unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   "actor start authority is unavailable",
			retryable: true,
		})
	}
}

// actorStartFailure is the failure a run-sourced Actor start reports to the
// worker in a 200 response, or false when the error is not one: a committed
// Session rejection, an expired or conflicting idempotency claim, and the
// key, deployment, Computer, Secret and request rejections.
func actorStartFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var operation *session.OperationError
	if errors.As(err, &operation) {
		return runtimeOperationFailure(operation.Code, operation.Code, false), true
	}
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return workerapi.RuntimeOperationFailure{Code: expired.ErrorCode(), Message: expired.Error()}, true
	}
	var claimConflict idempotency.ConflictError
	var keyConflict session.KeyConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeOperationFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor start", false), true
	case errors.As(err, &keyConflict):
		return runtimeOperationFailure("actor_key_conflict", keyConflict.Error(), false), true
	case errors.Is(err, session.ErrActorNotDeployed):
		return runtimeOperationFailure("actor_not_deployed", err.Error(), false), true
	case errors.Is(err, session.ErrStartComputerNotFound):
		return runtimeOperationFailure("computer_not_found", err.Error(), false), true
	case errors.Is(err, session.ErrStartComputerUnavailable):
		return runtimeOperationFailure("computer_unavailable", err.Error(), true), true
	case errors.Is(err, session.ErrStartSecretUnavailable):
		return runtimeOperationFailure("secret_unavailable", err.Error(), false), true
	case errors.Is(err, session.ErrStartInvalid):
		return runtimeOperationFailure("invalid_actor_start", err.Error(), false), true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

// writeWorkerActorStartError writes a run-sourced Actor start's failure. A
// stale source or stale credential claims are never reported as a failure
// body; an internal failure is logged with its cause.
func (s *Server) writeWorkerActorStartError(w http.ResponseWriter, correlationID, leaseID string, err error) {
	if !errors.Is(err, run.ErrStaleSource) && !errors.Is(err, workergroup.ErrStaleClaims) {
		if failure, ok := actorStartFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.StartActorResponse{CorrelationID: correlationID, Failed: &failure})
			return
		}
	}
	mapped := sessionError(err, sessionWorkerStartOperation)
	if errorStatus(mapped) == http.StatusInternalServerError {
		s.log.Error("start run-sourced Actor", "run_lease_id", leaseID, "error", err)
	}
	writeError(w, mapped)
}
