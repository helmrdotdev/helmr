package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// sessionOperation selects the vocabulary of a Session operation's errors.
type sessionOperation int

const (
	// sessionPublicOperation is a public Session command or a Session event
	// or Turn read.
	sessionPublicOperation sessionOperation = iota + 1
	sessionGetOperation
	sessionListOperation
	sessionStartOperation
	sessionWorkerStartOperation
)

// sessionError maps a session owner error to the API error the caller is
// told about. A public Session operation reports an expired claim as gone,
// passes transport errors through, and maps idempotency conflicts and
// committed Session rejections by code; a missing Session, including one
// whose Computer cannot be locked for admission, is not found, and other
// errors are internal. Session reads report a missing Session as not found
// and other errors as retryable unavailability. A public Actor start the
// session owner could not admit for an undescribed reason is retryable
// unavailability. A run-sourced Actor start asks the worker to
// re-authenticate on stale credential claims and reports a stale source as a
// conflict; its other undescribed errors are internal.
func sessionError(err error, operation sessionOperation) error {
	switch operation {
	case sessionPublicOperation:
		return sessionPublicError(err)
	case sessionGetOperation, sessionListOperation:
		if operation == sessionGetOperation && errors.Is(err, session.ErrNotFound) {
			return notFound(codedError{code: "session_not_found", message: "session was not found"})
		}
		return unavailable(codedError{code: "session_authority_unavailable", message: "Session authority is unavailable", retryable: true})
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

func sessionPublicError(err error) error {
	var expired idempotency.ExpiredError
	var transport apiError
	var collision idempotency.ConflictError
	var operation *session.OperationError
	switch {
	case errors.As(err, &expired):
		return gone(expired)
	case errors.As(err, &transport):
		return err
	case errors.As(err, &collision):
		return conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier operation"})
	case errors.As(err, &operation):
		coded := codedError{code: operation.Code, message: operation.Code}
		switch operation.Code {
		case "session_not_found", "turn_not_found":
			return notFound(coded)
		case "invalid_request", "invalid_cursor":
			return badRequest(coded)
		case "forbidden":
			return forbidden(coded)
		case "cursor_expired":
			return gone(sessionCursorExpiredError{codedError: coded, retainedAfter: operation.RetainedAfter})
		default:
			return conflict(coded)
		}
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, session.ErrComputerAuthority):
		return notFound(codedError{code: "session_not_found", message: "Session not found"})
	default:
		return errors.New("session operation failed")
	}
}

type sessionCursorExpiredError struct {
	codedError
	retainedAfter int64
}

func (e sessionCursorExpiredError) ErrorDetails() map[string]json.RawMessage {
	return map[string]json.RawMessage{"retained_after": json.RawMessage(strconv.FormatInt(e.retainedAfter, 10))}
}

// writeSessionError writes a public Session operation's failure. It logs the
// cause of an internal public operation failure and of an unavailable
// Session read.
func (s *Server) writeSessionError(w http.ResponseWriter, err error, operation sessionOperation) {
	mapped := sessionError(err, operation)
	switch status := errorStatus(mapped); {
	case operation == sessionPublicOperation && status == http.StatusInternalServerError && s.log != nil:
		s.log.Error("session operation failed", "error", err)
	case (operation == sessionGetOperation || operation == sessionListOperation) && status == http.StatusServiceUnavailable:
		s.log.Error("read Session failed", "error", err)
	}
	writeError(w, mapped)
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
