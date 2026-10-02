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
	// sessionWorkerOperation is a worker Session command that answers 200
	// with its failure: send, enqueue, Turn message, close, event read,
	// cancel, interrupt, resume, Turn retrieve, Turn message readiness, claim
	// and completion, settlement begin, Session output and control poll.
	sessionWorkerOperation
	sessionWorkerOutputOperation
	sessionWorkerCommitOperation
	sessionWorkerCompleteOperation
	sessionWorkerWaitOperation
)

// sessionError maps a session owner error to the API error the caller is
// told about. A public Session operation reports an expired claim as gone,
// passes transport errors through, and maps idempotency conflicts and
// committed Session rejections by code; a missing Session, including one
// whose Computer cannot be locked for admission, is not found, and other
// errors are internal. Session reads report a missing Session as not found
// and other errors as retryable unavailability. A public Actor start the
// session owner could not admit for an undescribed reason is retryable
// unavailability. Stored Actor start authority failures are internal.
// A run-sourced Actor start asks the worker to
// re-authenticate on stale credential claims and reports a stale source as a
// conflict; its other undescribed errors are internal.
//
// Worker Session operations ask the worker to re-authenticate on stale
// credential claims first. A worker Session command reports a stale
// execution as a conflict carrying the error and passes every other error
// through as internal, including inconsistent Session authority and
// unavailable Secret deliveries. Turn output, Turn commit, Actor completion
// and Actor input wait report their stale receipt as a conflict with a
// fixed message; an Actor completion still cleaning up owned executions is
// unavailable, and one with unavailable Secret deliveries or a rejected
// constraint is unprocessable. Their other errors are internal.
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
	case sessionWorkerOperation, sessionWorkerOutputOperation, sessionWorkerCommitOperation, sessionWorkerCompleteOperation, sessionWorkerWaitOperation:
		if errors.Is(err, workergroup.ErrStaleClaims) {
			return unauthorized(errors.New("worker authentication is required"))
		}
		return sessionWorkerError(err, operation)
	}
	return errors.New("session operation failed")
}

func sessionWorkerError(err error, operation sessionOperation) error {
	switch operation {
	case sessionWorkerOperation:
		if errors.Is(err, session.ErrStaleOutput) || errors.Is(err, session.ErrStaleExecution) {
			return conflict(err)
		}
		return err
	case sessionWorkerOutputOperation:
		if errors.Is(err, session.ErrStaleOutput) {
			return conflict(session.ErrStaleOutput)
		}
		return errors.New("append actor output")
	case sessionWorkerCommitOperation:
		if errors.Is(err, session.ErrStaleTurnCommit) {
			return conflict(session.ErrStaleTurnCommit)
		}
		return errors.New("commit actor turn")
	case sessionWorkerCompleteOperation:
		switch {
		case errors.Is(err, session.ErrStopCleanupPending):
			return unavailable(err)
		case errors.Is(err, session.ErrStaleCompletion):
			return conflict(session.ErrStaleCompletion)
		case errors.Is(err, session.ErrCompletionAdmission), isDeterministicWorkerAdmission(err):
			return apiError{kind: errUnprocessable, err: errors.New("actor completion admission is invalid")}
		}
		return errors.New("complete actor")
	default:
		if errors.Is(err, session.ErrStaleExecution) || errors.Is(err, run.ErrWaitCursor) || errors.Is(err, session.ErrAuthority) ||
			errors.Is(err, run.ErrTurnStopped) || errors.Is(err, run.ErrTurnScope) {
			return conflict(errors.New("worker actor input wait receipt is stale"))
		}
		return errors.New("register worker actor input wait")
	}
}

// sessionWorkerFailure is the failure a worker Session command, Turn output
// or child Task invocation reports to the worker in a 200 response, or false
// when the error is not one: an expired or conflicting idempotency claim, a
// committed Session rejection, a settling, stopping or inactive Turn, a
// stale producer scope and oversized output.
func sessionWorkerFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return workerapi.RuntimeOperationFailure{Code: expired.ErrorCode(), Message: expired.Error()}, true
	}
	var conflictError idempotency.ConflictError
	var operation *session.OperationError
	switch {
	case errors.As(err, &operation):
		return runtimeOperationFailure(operation.Code, operation.Error(), false), true
	case errors.Is(err, run.ErrTurnUnsettled):
		// The operation code is also the message, as for the equivalent
		// OperationError.
		return runtimeOperationFailure("turn_unsettled", "turn_unsettled", false), true
	case errors.Is(err, run.ErrTurnStopped):
		return workerapi.RuntimeOperationFailure{Code: "turn_stopping", Message: err.Error()}, true
	case errors.Is(err, run.ErrTurnNotActive):
		return workerapi.RuntimeOperationFailure{Code: "turn_not_active", Message: err.Error()}, true
	case errors.Is(err, run.ErrTurnScope):
		return workerapi.RuntimeOperationFailure{Code: "stale_execution", Message: err.Error()}, true
	case errors.As(err, &conflictError):
		return workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor output",
		}, true
	case errors.Is(err, session.ErrOutputTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "actor_output_too_large", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

// writeWorkerSessionCommand writes a worker Session command's outcome: 200
// accepted, 200 with its failure, where a stale source is a stale_execution
// failure, or the mapped error. It logs the cause of an internal failure.
func (s *Server) writeWorkerSessionCommand(w http.ResponseWriter, correlation string, err error) {
	if errors.Is(err, run.ErrStaleSource) {
		err = &session.OperationError{Code: "stale_execution"}
	}
	response := workerapi.TurnCommandResponse{CorrelationID: correlation, Accepted: err == nil}
	if err != nil {
		failure, ok := sessionWorkerFailure(err)
		if !ok {
			mapped := sessionError(err, sessionWorkerOperation)
			if errorStatus(mapped) == http.StatusInternalServerError {
				s.log.Error("Session worker command", "error", err)
			}
			writeError(w, mapped)
			return
		}
		response.Failed = &failure
	}
	writeJSON(w, http.StatusOK, response)
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
	case (operation == sessionPublicOperation || operation == sessionStartOperation) && status == http.StatusInternalServerError && s.log != nil:
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
		return errors.New("actor start stored authority is invalid")
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
	case errors.Is(err, computer.ErrPreparationExhausted):
		return runtimeOperationFailure("computer_preparation_exhausted", "Computer preparation limit reached", false), true
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
