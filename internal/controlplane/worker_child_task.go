package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

var (
	errChildTaskInvokeUnsupported = errors.New("child task invocation method is unsupported")
)

type childTaskInvokeFailurePoint string

const (
	childTaskInvokePointLoadLease   childTaskInvokeFailurePoint = "load_live_run_lease"
	childTaskInvokePointSourceScope childTaskInvokeFailurePoint = "source_scope"
	childTaskInvokePointTransaction childTaskInvokeFailurePoint = "transaction_authority"
)

type childTaskOptions struct {
	Queue          string                     `json:"queue,omitempty"`
	ConcurrencyKey *string                    `json:"concurrency_key,omitempty"`
	Priority       int32                      `json:"priority,omitempty"`
	TTL            string                     `json:"ttl,omitempty"`
	Retry          *api.StartActorRetryPolicy `json:"retry,omitempty"`
	Metadata       json.RawMessage            `json:"metadata,omitempty"`
	Tags           []string                   `json:"tags,omitempty"`
}

func (s *Server) workerInvokeChildTask(w http.ResponseWriter, r *http.Request) {
	var request workerapi.InvokeChildTaskRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Method != "start" && request.Method != "call" {
		writeError(w, badRequest(codedError{
			code: "child_task_method_unsupported", message: errChildTaskInvokeUnsupported.Error(),
		}))
		return
	}
	correlationID, err := ids.Parse(request.CorrelationID)
	if err != nil {
		writeError(w, badRequest(errors.New("correlation_id must be a canonical UUIDv7")))
		return
	}
	var runWaitID uuid.UUID
	var resumeAttachID uuid.UUID
	if request.Method == "call" {
		runWaitID, err = ids.Parse(request.RunWaitID)
		if err != nil {
			writeError(w, badRequest(errors.New("run_wait_id must be a canonical UUIDv7 for task.call()")))
			return
		}
		resumeAttachID, err = ids.Parse(request.ResumeAttachID)
		if err != nil {
			writeError(w, badRequest(errors.New("resume_attach_id must be a canonical UUIDv7 for task.call()")))
			return
		}
		if correlationID == runWaitID || correlationID == resumeAttachID || runWaitID == resumeAttachID {
			writeError(w, badRequest(errors.New("correlation_id, run_wait_id, and resume_attach_id must be distinct")))
			return
		}
	} else if request.RunWaitID != "" || request.ResumeAttachID != "" {
		writeError(w, badRequest(errors.New("task.start() must not include run_wait_id or resume_attach_id")))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	request.IdempotencyKey = idempotencyKey
	if request.Method == "call" && request.IdempotencyKey == "" {
		writeError(w, badRequest(codedError{
			code: "invalid_idempotency_key", message: "task.call() requires an idempotency key",
		}))
		return
	}
	worker := workerFromContext(r.Context())
	fence := workerExecutionFence(worker, parsed, request.Lease)
	locators, err := run.LocateChildInvocation(r.Context(), s.db, fence)
	if err != nil {
		if !errors.Is(err, run.ErrChildInvokeStale) {
			s.writeChildTaskInvokeError(w, request.CorrelationID, err)
			return
		}
		stale := childTaskInvokeStaleAt(childTaskInvokePointLoadLease, run.ErrChildInvokeStale)
		s.log.Warn(
			"reject stale child Task invocation",
			"failure_point", childTaskInvokePointLoadLease,
			"run_lease_id", request.Lease.ID,
			"lease_sequence", request.Lease.LeaseSequence,
			"worker_host_id", worker.HostID,
		)
		writeError(w, conflict(stale))
		return
	}
	normalized, err := normalizeWorkerChildTaskRequest(request, locators)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	turnID, runGeneration, err := run.ParseWaitTurn(request.TurnID, request.RunGeneration)
	if err != nil {
		s.writeChildTaskInvokeError(w, request.CorrelationID, err)
		return
	}
	result, err := run.InvokeChild(r.Context(), s.tx, run.ChildInvoke{
		Fence: fence, Method: request.Method,
		SourceComputerID: pgvalue.MustUUIDValue(locators.ComputerID),
		Task:             normalized.runTaskStart(),
		IdempotencyKey:   normalized.IdempotencyKey,
		Fingerprint: idempotency.TaskChildInvokeFingerprint{
			Method: request.Method, PayloadPresent: normalized.PayloadPresent,
			Payload: normalized.Payload, Computer: normalized.fingerprint.Computer,
			QueueName: normalized.QueueName, ConcurrencyKey: normalized.ConcurrencyKey,
			Priority: normalized.Priority, QueuedTTLMS: normalized.QueuedTTLMS,
			RetryPolicy: normalized.RetryPolicy, Metadata: normalized.Metadata,
			Tags: normalized.Tags,
		},
		Cursor: request.ActorSpeculativeInputSequence,
		TurnID: turnID, RunGeneration: runGeneration,
		RunWaitID: runWaitID, ResumeAttachID: resumeAttachID,
		ChildResult: childTaskResult,
	})
	if err != nil {
		s.writeChildTaskInvokeError(w, request.CorrelationID, err)
		return
	}
	response := workerapi.InvokeChildTaskResponse{CorrelationID: request.CorrelationID}
	if call := result.Call; call != nil {
		opened := workerapi.CreateRunWaitResponse{
			RunID: pgvalue.UUIDString(call.ParentRunID), RunWaitID: call.RunWaitID.String(),
			ResumeAttachID:     call.ResumeAttachID.String(),
			ComputerInstanceID: pgvalue.UUIDString(call.ComputerInstanceID),
			WorkerEpoch:        call.WorkerEpoch,
		}
		if call.Completed {
			opened.ResolutionKind = "completed"
			opened.Resolution = call.Resolution
		}
		response.OpenedWait = &opened
	}
	if result.RunID != uuid.Nil() {
		response.Completed = &workerapi.ChildTaskStartResult{RunID: result.RunID.String()}
	}
	writeJSON(w, http.StatusOK, response)
}

func normalizeWorkerChildTaskRequest(
	request workerapi.InvokeChildTaskRequest,
	locators db.GetLiveRunLeaseLocatorsRow,
) (normalizedTaskStart, error) {
	if err := api.ValidateDefinitionID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, err
	}
	var computer api.ComputerIDTarget
	if err := decodeClosedJSON(request.Computer, &computer); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid computer: %w", err)
	}
	computerID, err := ids.Parse(computer.ID)
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid computer: %w", err)
	}
	var options childTaskOptions
	if err := decodeClosedJSON(request.Options, &options); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid options: %w", err)
	}
	ttl, retry, err := taskStartPolicyFromAPI(api.StartTaskRequest{
		TTL: options.TTL, Retry: options.Retry,
	})
	if err != nil {
		return normalizedTaskStart{}, err
	}
	return normalizeTaskStart(taskStartRequest{
		OrgID: pgvalue.MustUUIDValue(locators.OrgID), ProjectID: pgvalue.MustUUIDValue(locators.ProjectID),
		EnvironmentID:  pgvalue.MustUUIDValue(locators.EnvironmentID),
		TaskDeclaredID: request.TaskDeclaredID, PayloadPresent: request.PayloadPresent,
		Payload: request.Payload, ComputerID: computerID, IdempotencyKey: request.IdempotencyKey,
		QueueName: options.Queue, ConcurrencyKey: options.ConcurrencyKey, Priority: options.Priority,
		QueuedTTLMS: ttl, RetryPolicy: retry, Metadata: options.Metadata, Tags: options.Tags,
	})
}

// writeChildTaskInvokeError writes a child Task invocation's failure: a
// rejection the worker handles is a 200 failure; stale claims, a stale
// invocation and unavailability are errors.
func (s *Server) writeChildTaskInvokeError(w http.ResponseWriter, correlationID string, err error) {
	if !errors.Is(err, workergroup.ErrStaleClaims) {
		if failure, ok := childInvokeFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.InvokeChildTaskResponse{CorrelationID: correlationID, Failed: &failure})
			return
		}
	}
	mapped := runError(err, runChildInvokeOperation)
	switch errorStatus(mapped) {
	case http.StatusConflict:
		if point, ok := staleAuthorityPointOf(mapped); ok {
			s.log.Warn(
				"reject stale child Task invocation",
				"failure_point", point,
				"correlation_id", correlationID,
			)
		}
	case http.StatusServiceUnavailable:
		s.log.Error("invoke child Task", "error", err)
	}
	writeError(w, mapped)
}
