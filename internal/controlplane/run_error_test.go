package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRunErrorMapsWorkerOperations(t *testing.T) {
	claims := errors.Join(run.ErrStale, workergroup.ErrStaleClaims)
	unavailable := errors.New("database is down")
	checkViolation := &pgconn.PgError{Code: "23514"}
	for _, test := range []struct {
		name      string
		operation runOperation
		err       error
		status    int
		code      string
		message   string
		point     string
	}{
		{"discovery claims", runLeaseDiscoveryOperation, workergroup.ErrStaleClaims, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"discovery failure", runLeaseDiscoveryOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"claim claims first", runLeaseClaimOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"claim stale", runLeaseClaimOperation, run.ErrStale, http.StatusConflict, "conflict", "run lease claim is stale", ""},
		{"claim no rows", runLeaseClaimOperation, pgx.ErrNoRows, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"claim failure", runLeaseClaimOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"start claims first", runStartOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"start stale", runStartOperation, run.ErrStale, http.StatusConflict, "run_start_stale", "run start authority is stale", "execution"},
		{"start failure", runStartOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"entrypoint claims first", runEntrypointOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"entrypoint stale", runEntrypointOperation, run.ErrStale, http.StatusConflict, "conflict", "run entrypoint acknowledgement is stale", ""},
		{"entrypoint failure", runEntrypointOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"renewal claims first", runLeaseRenewalOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"renewal stale", runLeaseRenewalOperation, run.ErrStale, http.StatusConflict, "conflict", "worker run lease fence is stale", ""},
		{"renewal failure", runLeaseRenewalOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"finalization claims first", runFinalizationOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"finalization stale", runFinalizationOperation, errors.Join(run.ErrStale, unavailable), http.StatusConflict, "conflict", "run finalization authority is stale", ""},
		{"finalization secret lock", runFinalizationOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"completion claims first", runTaskCompletionOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"completion stale", runTaskCompletionOperation, errors.Join(run.ErrStale, pgx.ErrNoRows), http.StatusConflict, "task_completion_stale", "task completion authority is stale", "execution"},
		{"completion replay", runTaskCompletionOperation, run.ErrTaskCompletionReplayDiffers, http.StatusConflict, "task_completion_stale", "task completion authority is stale", "replay"},
		{"completion admission", runTaskCompletionOperation, run.ErrTaskCompletionAdmission, http.StatusUnprocessableEntity, "unprocessable_entity", "task completion admission is invalid", ""},
		{"completion check violation", runTaskCompletionOperation, checkViolation, http.StatusUnprocessableEntity, "unprocessable_entity", "task completion admission is invalid", ""},
		{"completion failure", runTaskCompletionOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"log claims", runLogAppendOperation, workergroup.ErrStaleClaims, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"log no rows", runLogAppendOperation, pgx.ErrNoRows, http.StatusConflict, "conflict", "worker run lease is stale or the log chunk sequence contains different content", ""},
		{"log differs", runLogAppendOperation, run.ErrLogChunkDiffers, http.StatusConflict, "conflict", "worker log chunk sequence already contains different content", ""},
		{"log failure", runLogAppendOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"structured log no rows", runStructuredLogAppendOperation, pgx.ErrNoRows, http.StatusConflict, "conflict", "worker run lease is stale or the structured log sequence contains different content", ""},
		{"structured log differs", runStructuredLogAppendOperation, run.ErrLogChunkDiffers, http.StatusConflict, "conflict", "structured log sequence already contains different content", ""},
		{"structured log failure", runStructuredLogAppendOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"metadata claims first", runMetadataOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"metadata expired", runMetadataOperation, idempotency.ExpiredError{}, http.StatusGone, "operation_expired", idempotency.ExpiredError{}.Error(), ""},
		{"metadata idempotency conflict", runMetadataOperation, idempotency.ConflictError{}, http.StatusConflict, "conflict", idempotency.ConflictError{}.Error(), ""},
		{"metadata stale", runMetadataOperation, run.ErrStale, http.StatusConflict, "conflict", "worker run lease fence is stale", ""},
		{"metadata no rows", runMetadataOperation, pgx.ErrNoRows, http.StatusConflict, "conflict", "worker run lease fence is stale", ""},
		{"metadata rejected", runMetadataOperation, errors.New(`run metadata key "steps" is not a finite number`), http.StatusUnprocessableEntity, "run_metadata_rejected", `run metadata key "steps" is not a finite number`, ""},
		{"wait resume claims first", runWaitResumeOperation, errors.Join(pgx.ErrNoRows, workergroup.ErrStaleClaims), http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"wait resume no rows", runWaitResumeOperation, pgx.ErrNoRows, http.StatusConflict, "conflict", "run wait resume acknowledgement is stale", ""},
		{"wait resume failure", runWaitResumeOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"task start claims are not compared", runTaskStartOperation, workergroup.ErrStaleClaims, http.StatusServiceUnavailable, "task_start_authority_unavailable", "task start authority is unavailable", ""},
		{"task start expired", runTaskStartOperation, fmt.Errorf("start: %w", idempotency.ExpiredError{}), http.StatusGone, "operation_expired", idempotency.ExpiredError{}.Error(), ""},
		{"task start preparation exhausted", runTaskStartOperation, run.ErrComputerPreparationExhausted, http.StatusConflict, "computer_preparation_exhausted", "Computer preparation limit reached", ""},
		{"task start idempotency conflict", runTaskStartOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier task start", ""},
		{"task start not deployed", runTaskStartOperation, run.ErrTaskNotDeployed, http.StatusNotFound, "task_not_deployed", "task declaration is not deployed", ""},
		{"task start computer unavailable", runTaskStartOperation, run.ErrTaskComputerUnavailable, http.StatusConflict, "computer_unavailable", "task start computer cannot accept execution", ""},
		{"task start secret unavailable", runTaskStartOperation, run.ErrTaskSecretUnavailable, http.StatusConflict, "secret_unavailable", "task start computer secret is unavailable", ""},
		{"task start payload presence", runTaskStartOperation, run.ErrTaskPayloadPresenceInvalid, http.StatusBadRequest, "invalid_task_start", "task payload presence does not match its declaration", ""},
		{"task start invalid", runTaskStartOperation, fmt.Errorf("%w: retry is invalid", run.ErrTaskStartInvalid), http.StatusBadRequest, "invalid_task_start", "task start request is invalid: retry is invalid", ""},
		{"task start receipt", runTaskStartOperation, run.ErrTaskStartReceiptInvalid, http.StatusServiceUnavailable, "task_start_authority_unavailable", "task start authority is unavailable", ""},
		{"child invoke claims first", runChildInvokeOperation, errors.Join(run.ErrChildInvokeStale, workergroup.ErrStaleClaims), http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"child invoke stale", runChildInvokeOperation, run.ErrChildInvokeStale, http.StatusConflict, "child_task_invoke_stale", "child task invocation authority is stale", "transaction_authority"},
		{"child invoke stale source", runChildInvokeOperation, run.ErrChildInvokeSourceScope, http.StatusConflict, "child_task_invoke_stale", "child task invocation authority is stale", "source_scope"},
		{"child invoke failure", runChildInvokeOperation, unavailable, http.StatusServiceUnavailable, "child_task_invoke_authority_unavailable", "child task invocation authority is unavailable", ""},
		{"timer wait claims first", runTimerWaitOperation, errors.Join(run.ErrStale, workergroup.ErrStaleClaims), http.StatusUnauthorized, "unauthorized", "worker authentication is required", ""},
		{"timer wait stale", runTimerWaitOperation, run.ErrStale, http.StatusConflict, "conflict", "worker timer wait receipt is stale", ""},
		{"timer wait cursor", runTimerWaitOperation, run.ErrWaitCursor, http.StatusConflict, "conflict", "worker timer wait receipt is stale", ""},
		{"timer wait turn stopped", runTimerWaitOperation, run.ErrTurnStopped, http.StatusConflict, "conflict", "worker timer wait receipt is stale", ""},
		{"timer wait turn scope", runTimerWaitOperation, run.ErrTurnScope, http.StatusConflict, "conflict", "worker timer wait receipt is stale", ""},
		{"timer wait turn not active", runTimerWaitOperation, run.ErrTurnNotActive, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"timer wait no rows", runTimerWaitOperation, pgx.ErrNoRows, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"wait poll claims are not compared", runWaitPollOperation, workergroup.ErrStaleClaims, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"wait poll not found", runWaitPollOperation, run.ErrWaitNotFound, http.StatusConflict, "conflict", "worker run wait is stale", ""},
		{"wait poll fence", runWaitPollOperation, run.ErrWaitFenceStale, http.StatusConflict, "conflict", "worker run wait fence is stale", ""},
		{"wait poll turn revoked", runWaitPollOperation, run.ErrWaitTurnRevoked, http.StatusConflict, "conflict", "turn wait authority was revoked", ""},
		{"wait poll failure", runWaitPollOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", ""},
		{"get not found", runGetOperation, run.ErrNotFound, http.StatusNotFound, "run_not_found", "run not found", ""},
		{"get failure", runGetOperation, unavailable, http.StatusServiceUnavailable, "run_authority_unavailable", "run authority is unavailable", ""},
		{"list failure", runListOperation, unavailable, http.StatusServiceUnavailable, "run_authority_unavailable", "run authority is unavailable", ""},
		{"list not found is unavailable", runListOperation, run.ErrNotFound, http.StatusServiceUnavailable, "run_authority_unavailable", "run authority is unavailable", ""},
		{"cancel not found", runCancelOperation, run.ErrCancellationNotFound, http.StatusNotFound, "run_not_found", "run not found", ""},
		{"cancel lifecycle conflict", runCancelOperation, run.ErrCancellationConflict, http.StatusConflict, "run_lifecycle_conflict", "run already has another terminal outcome", ""},
		{"cancel expired", runCancelOperation, idempotency.ExpiredError{}, http.StatusGone, "operation_expired", idempotency.ExpiredError{}.Error(), ""},
		{"cancel rejection", runCancelOperation, &run.CancellationRejectionError{Code: "session_closed"}, http.StatusConflict, "session_closed", "session_closed", ""},
		{"cancel idempotency conflict", runCancelOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier operation", ""},
		{"cancel claims are not compared", runCancelOperation, workergroup.ErrStaleClaims, http.StatusServiceUnavailable, "run_cancellation_unavailable", "run cancellation is unavailable", ""},
		{"cancel failure", runCancelOperation, unavailable, http.StatusServiceUnavailable, "run_cancellation_unavailable", "run cancellation is unavailable", ""},
		{"task start failure", runTaskStartOperation, unavailable, http.StatusServiceUnavailable, "task_start_authority_unavailable", "task start authority is unavailable", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, runError(test.err, test.operation))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Message != test.message || (test.code != "" && body.Error.Code != test.code) {
				t.Fatalf("error = %s %q, want %s %q", body.Error.Code, body.Error.Message, test.code, test.message)
			}
			var point string
			if raw, ok := body.Error.Details["point"]; ok {
				if err := json.Unmarshal(raw, &point); err != nil {
					t.Fatal(err)
				}
			}
			if point != test.point {
				t.Fatalf("point = %q, want %q", point, test.point)
			}
		})
	}
}

func TestWriteRunErrorLogsStalePointsWithoutTheirCause(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	worker := workergroup.HostPrincipal{Epoch: 3}
	lease := workerapi.RunLeaseFence{ID: "lease", LeaseSequence: 2}
	cause := errors.Join(run.ErrStale, errors.New("credential=secret-sentinel"))
	server.writeRunError(httptest.NewRecorder(), cause, runStartOperation, worker, lease)
	server.writeRunError(httptest.NewRecorder(), run.ErrTaskCompletionReplayDiffers, runTaskCompletionOperation, worker, lease)
	for _, want := range []string{`"msg":"run start acknowledgement is stale"`, `"msg":"task completion receipt rejected"`, `"failure_point":"execution"`, `"failure_point":"replay"`, `"run_lease_id":"lease"`, `"lease_sequence":2`, `"worker_epoch":3`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %s: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "secret-sentinel") || strings.Contains(logs.String(), `"error"`) {
		t.Fatalf("stale point logs included their cause: %s", logs.String())
	}
}

func TestChildInvokeFailureReportsWorkerHandledRejections(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want workerapi.RuntimeOperationFailure
	}{
		{"expired", idempotency.ExpiredError{}, workerapi.RuntimeOperationFailure{Code: "operation_expired", Message: idempotency.ExpiredError{}.Error()}},
		{"idempotency conflict", idempotency.ConflictError{}, workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor output"}},
		{"turn scope", run.ErrTurnScope, workerapi.RuntimeOperationFailure{Code: "stale_execution", Message: run.ErrTurnScope.Error()}},
		{"not deployed", run.ErrTaskNotDeployed, workerapi.RuntimeOperationFailure{Code: "task_not_deployed", Message: "task declaration is not deployed"}},
		{"computer not found", run.ErrTaskComputerNotFound, workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: "task start computer was not found"}},
		{"computer unavailable", run.ErrTaskComputerUnavailable, workerapi.RuntimeOperationFailure{Code: "computer_unavailable", Message: "task start computer cannot accept execution", Retryable: true}},
		{"secret unavailable", run.ErrTaskSecretUnavailable, workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: "task start computer secret is unavailable"}},
		{"secret target", computer.ErrSecretUnavailable, workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: computer.ErrSecretUnavailable.Error()}},
		{"payload presence", run.ErrTaskPayloadPresenceInvalid, workerapi.RuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: "task payload presence does not match its declaration"}},
		{"invalid", run.ErrTaskStartInvalid, workerapi.RuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: "task start request is invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := childInvokeFailure(test.err)
			if !ok || got != test.want {
				t.Fatalf("failure = %+v %v, want %+v", got, ok, test.want)
			}
		})
	}
	for _, err := range []error{run.ErrChildInvokeStale, run.ErrTaskStartReceiptInvalid, errors.New("database is down")} {
		if got, ok := childInvokeFailure(err); ok {
			t.Fatalf("%v reported failure %+v", err, got)
		}
	}
}
