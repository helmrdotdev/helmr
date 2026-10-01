package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// runOperation selects the vocabulary of a Run operation's errors.
type runOperation int

const (
	runLeaseDiscoveryOperation runOperation = iota + 1
	runLeaseClaimOperation
	runStartOperation
	runEntrypointOperation
	runLeaseRenewalOperation
	runFinalizationOperation
	runTaskCompletionOperation
	runLogAppendOperation
	runStructuredLogAppendOperation
	runMetadataOperation
	runWaitResumeOperation
	runTaskStartOperation
	runChildInvokeOperation
	runTimerWaitOperation
	runWaitPollOperation
	runListOperation
	runGetOperation
	runCancelOperation
)

// claims reports whether the operation compares the worker's credential
// claims. Discovery and log appends fence the lease only in their
// statements; public operations have no worker claims.
func (o runOperation) claims() bool {
	switch o {
	case runLeaseDiscoveryOperation, runLogAppendOperation, runStructuredLogAppendOperation, runWaitPollOperation,
		runTaskStartOperation, runListOperation, runGetOperation, runCancelOperation:
		return false
	default:
		return true
	}
}

// runStale is the conflict each operation reports when its receipt no longer
// addresses the live execution.
var runStale = map[runOperation]string{
	runLeaseClaimOperation:          "run lease claim is stale",
	runEntrypointOperation:          "run entrypoint acknowledgement is stale",
	runLeaseRenewalOperation:        "worker run lease fence is stale",
	runFinalizationOperation:        "run finalization authority is stale",
	runLogAppendOperation:           "worker run lease is stale or the log chunk sequence contains different content",
	runStructuredLogAppendOperation: "worker run lease is stale or the structured log sequence contains different content",
	runMetadataOperation:            "worker run lease fence is stale",
	runWaitResumeOperation:          "run wait resume acknowledgement is stale",
	runTimerWaitOperation:           "worker timer wait receipt is stale",
}

// runLogDiffers is the conflict each log append reports for a sequence that
// already holds a different chunk.
var runLogDiffers = map[runOperation]string{
	runLogAppendOperation:           "worker log chunk sequence already contains different content",
	runStructuredLogAppendOperation: "structured log sequence already contains different content",
}

// runFailed names each operation's internal failure: the public error and
// its log message.
var runFailed = map[runOperation]struct{ public, log string }{
	runLeaseDiscoveryOperation:      {"discover worker run leases", "discover worker run leases failed"},
	runLeaseClaimOperation:          {"serve worker run lease claim", "serve worker Run Lease claim failed"},
	runStartOperation:               {"start run", "start Run failed"},
	runEntrypointOperation:          {"enter run entrypoint", "enter Run entrypoint failed"},
	runLeaseRenewalOperation:        {"renew worker run lease", "renew worker Run Lease failed"},
	runFinalizationOperation:        {"begin run finalization", "begin Run finalization failed"},
	runTaskCompletionOperation:      {"complete task", "complete Task failed"},
	runLogAppendOperation:           {"append worker logs", "append worker logs failed"},
	runStructuredLogAppendOperation: {"append structured run log", "append structured Run log failed"},
	runMetadataOperation:            {"update run metadata", "update Run metadata failed"},
	runWaitResumeOperation:          {"acknowledge run wait resume", "acknowledge Run wait resume failed"},
	runTimerWaitOperation:           {"register worker timer wait", "register worker timer Wait failed"},
	runWaitPollOperation:            {"load worker run wait", "load worker Run wait failed"},
}

// runStalePointLog is the warning an operation logs with the failure point
// of its stale receipt.
var runStalePointLog = map[runOperation]string{
	runStartOperation:          "run start acknowledgement is stale",
	runTaskCompletionOperation: "task completion receipt rejected",
}

// runError maps a run owner error to the API error the caller is told about.
// Stale credential claims ask a worker to re-authenticate before anything
// else is considered. A start or task completion receipt that is stale
// carries its failure point. Metadata rejections are 422 with their cause.
// A public Task start the run owner could not admit for another reason is
// retryable unavailability; errors any other operation does not describe are
// internal.
func runError(err error, operation runOperation) error {
	if operation.claims() && errors.Is(err, workergroup.ErrStaleClaims) {
		return unauthorized(errors.New("worker authentication is required"))
	}
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	switch operation {
	case runStartOperation:
		if errors.Is(err, run.ErrStale) {
			return conflict(&staleAuthorityError{operation: staleAuthorityRunStart, point: "execution", cause: err})
		}
	case runTaskCompletionOperation:
		switch {
		case errors.Is(err, run.ErrTaskCompletionReplayDiffers):
			return conflict(&staleAuthorityError{operation: staleAuthorityTaskCompletion, point: "replay", cause: err})
		case errors.Is(err, run.ErrStale):
			return conflict(&staleAuthorityError{operation: staleAuthorityTaskCompletion, point: "execution", cause: err})
		case errors.Is(err, run.ErrTaskCompletionAdmission), isDeterministicWorkerAdmission(err):
			return apiError{kind: errUnprocessable, err: errors.New("task completion admission is invalid")}
		}
	case runLogAppendOperation, runStructuredLogAppendOperation, runWaitResumeOperation:
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict(errors.New(runStale[operation]))
		}
		if errors.Is(err, run.ErrLogChunkDiffers) && runLogDiffers[operation] != "" {
			return conflict(errors.New(runLogDiffers[operation]))
		}
	case runMetadataOperation:
		switch {
		case errors.As(err, &expired):
			return gone(expired)
		case errors.As(err, &idempotencyConflict):
			return conflict(idempotencyConflict)
		case errors.Is(err, run.ErrStale), errors.Is(err, pgx.ErrNoRows):
			return conflict(errors.New(runStale[operation]))
		default:
			return apiError{kind: errUnprocessable, err: codedError{code: "run_metadata_rejected", message: err.Error()}}
		}
	case runTimerWaitOperation:
		if errors.Is(err, run.ErrStale) || errors.Is(err, run.ErrWaitCursor) || errors.Is(err, run.ErrTurnStopped) || errors.Is(err, run.ErrTurnScope) {
			return conflict(errors.New(runStale[operation]))
		}
	case runWaitPollOperation:
		switch {
		case errors.Is(err, run.ErrWaitNotFound):
			return conflict(errors.New("worker run wait is stale"))
		case errors.Is(err, run.ErrWaitFenceStale):
			return conflict(errors.New("worker run wait fence is stale"))
		case errors.Is(err, run.ErrWaitTurnRevoked):
			return conflict(errors.New("turn wait authority was revoked"))
		}
	case runTaskStartOperation:
		return taskStartError(err)
	case runListOperation, runGetOperation:
		if operation == runGetOperation && errors.Is(err, run.ErrNotFound) {
			return notFound(codedError{code: "run_not_found", message: "run not found"})
		}
		return unavailable(codedError{
			code: "run_authority_unavailable", message: "run authority is unavailable", retryable: true,
		})
	case runCancelOperation:
		return cancelError(err)
	case runChildInvokeOperation:
		switch {
		case errors.Is(err, run.ErrChildInvokeStale):
			point := childTaskInvokePointTransaction
			if errors.Is(err, run.ErrChildInvokeSourceScope) {
				point = childTaskInvokePointSourceScope
			}
			return conflict(childTaskInvokeStaleAt(point, err))
		default:
			return unavailable(codedError{
				code:    "child_task_invoke_authority_unavailable",
				message: "child task invocation authority is unavailable", retryable: true,
			})
		}
	default:
		if errors.Is(err, run.ErrStale) && runStale[operation] != "" {
			return conflict(errors.New(runStale[operation]))
		}
	}
	return errors.New(runFailed[operation].public)
}

func cancelError(err error) error {
	var rejection *run.CancellationRejectionError
	var expired idempotency.ExpiredError
	var collision idempotency.ConflictError
	switch {
	case errors.Is(err, run.ErrCancellationNotFound):
		return notFound(codedError{code: "run_not_found", message: "run not found"})
	case errors.Is(err, run.ErrCancellationConflict):
		return conflict(codedError{
			code: "run_lifecycle_conflict", message: "run already has another terminal outcome",
		})
	case errors.As(err, &expired):
		return gone(expired)
	case errors.As(err, &rejection):
		return conflict(codedError{code: rejection.Code, message: rejection.Code})
	case errors.As(err, &collision):
		return conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier operation"})
	default:
		return unavailable(codedError{
			code: "run_cancellation_unavailable", message: "run cancellation is unavailable",
			retryable: true,
		})
	}
}

func taskStartError(err error) error {
	var expired idempotency.ExpiredError
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.As(err, &expired):
		return gone(expired)
	case errors.Is(err, run.ErrComputerPreparationExhausted):
		return conflict(codedError{code: "computer_preparation_exhausted", message: "Computer preparation limit reached"})
	case errors.As(err, &idempotencyConflict):
		return conflict(codedError{
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with an earlier task start",
		})
	case errors.Is(err, run.ErrTaskNotDeployed):
		return notFound(codedError{code: "task_not_deployed", message: err.Error()})
	case errors.Is(err, run.ErrTaskComputerUnavailable):
		return conflict(codedError{code: "computer_unavailable", message: err.Error(), retryable: true})
	case errors.Is(err, run.ErrTaskSecretUnavailable):
		return conflict(codedError{code: "secret_unavailable", message: err.Error()})
	case errors.Is(err, run.ErrTaskPayloadPresenceInvalid), errors.Is(err, run.ErrTaskStartInvalid):
		return badRequest(codedError{code: "invalid_task_start", message: err.Error()})
	default:
		return unavailable(codedError{
			code:    "task_start_authority_unavailable",
			message: "task start authority is unavailable", retryable: true,
		})
	}
}

// childInvokeFailure is the failure a child Task invocation reports to the
// worker in a 200 response, or false when the error is not one. Worker
// Session failures are mapped first, including an expired or conflicting idempotency
// claim and Turn rejections; then deployment, Computer, Secret and request
// rejections.
func childInvokeFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	if failure, ok := sessionWorkerFailure(err); ok {
		return failure, true
	}
	switch {
	case errors.Is(err, run.ErrTaskNotDeployed):
		return workerapi.RuntimeOperationFailure{Code: "task_not_deployed", Message: err.Error()}, true
	case errors.Is(err, run.ErrTaskComputerNotFound):
		return workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: err.Error()}, true
	case errors.Is(err, run.ErrTaskComputerUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "computer_unavailable", Message: err.Error(), Retryable: true}, true
	case errors.Is(err, run.ErrTaskSecretUnavailable), errors.Is(err, computer.ErrSecretUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.Is(err, run.ErrTaskPayloadPresenceInvalid), errors.Is(err, run.ErrTaskStartInvalid):
		return workerapi.RuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

// writeRunError writes a worker Run lease operation's failure. It logs a
// stale receipt's failure point without the cause, and the cause of a
// failure the worker is not told about, of a rejected task completion
// admission and of a rejected metadata mutation.
func (s *Server) writeRunError(w http.ResponseWriter, err error, operation runOperation, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence) {
	mapped := runError(err, operation)
	status := errorStatus(mapped)
	switch {
	case status == http.StatusConflict && runStalePointLog[operation] != "":
		if point, ok := staleAuthorityPointOf(mapped); ok {
			s.log.Warn(
				runStalePointLog[operation],
				"failure_point", point,
				"run_lease_id", lease.ID,
				"lease_sequence", lease.LeaseSequence,
				"worker_group_id", worker.GroupID,
				"worker_host_id", worker.HostID,
				"worker_epoch", worker.Epoch,
			)
		}
	case status == http.StatusUnprocessableEntity && operation == runTaskCompletionOperation:
		s.log.Warn("task completion admission rejected", "run_lease_id", lease.ID, "error", err)
	case status == http.StatusUnprocessableEntity && operation == runMetadataOperation,
		status == http.StatusInternalServerError && operation != runLeaseDiscoveryOperation:
		s.log.Error(runFailed[operation].log, "run_lease_id", lease.ID, "error", err)
	case status == http.StatusInternalServerError:
		s.log.Error(runFailed[operation].log, "worker_host_id", worker.HostID.String(), "worker_epoch", worker.Epoch, "error", err)
	}
	writeError(w, mapped)
}
