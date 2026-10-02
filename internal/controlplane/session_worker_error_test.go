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
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSessionErrorMapsWorkerOperations(t *testing.T) {
	unavailable := errors.New("database is down")
	lost := errors.Join(session.ErrStaleOutput, pgx.ErrNoRows)
	claims := errors.Join(workergroup.ErrStaleClaims, session.ErrStaleOutput, session.ErrStaleExecution, session.ErrStaleTurnCommit, session.ErrStaleCompletion)
	cases := []sessionErrorCase{
		{"command claims first", sessionWorkerOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"command lost execution", sessionWorkerOperation, lost, http.StatusConflict, "conflict", lost.Error(), false},
		{"command stale output", sessionWorkerOperation, session.ErrStaleOutput, http.StatusConflict, "conflict", session.ErrStaleOutput.Error(), false},
		{"command stale execution", sessionWorkerOperation, session.ErrStaleExecution, http.StatusConflict, "conflict", session.ErrStaleExecution.Error(), false},
		{"command Session authority", sessionWorkerOperation, session.ErrAuthority, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"command Secret revoked", sessionWorkerOperation, secret.ErrDeliveryRevoked, http.StatusConflict, "conflict", "secret delivery is no longer authorized", false},
		{"output Secret revoked", sessionWorkerOutputOperation, secret.ErrDeliveryRevoked, http.StatusConflict, "conflict", "secret delivery is no longer authorized", false},
		{"commit Secret revoked", sessionWorkerCommitOperation, secret.ErrDeliveryRevoked, http.StatusConflict, "conflict", "secret delivery is no longer authorized", false},
		{"wait Secret revoked", sessionWorkerWaitOperation, secret.ErrDeliveryRevoked, http.StatusConflict, "conflict", "secret delivery is no longer authorized", false},
		{"command target changed", sessionWorkerOperation, run.ErrExecutionTargetChanged, http.StatusServiceUnavailable, "session_control_target_changed", "Session control target changed; retry the operation", true},
		{"command Secret delivery", sessionWorkerOperation, secret.ErrDeliveryUnavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"command Computer authority", sessionWorkerOperation, fmt.Errorf("%w: %w", session.ErrComputerAuthority, computer.ErrNotFound), http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"command stale source", sessionWorkerOperation, run.ErrStaleSource, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"command failure", sessionWorkerOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"output claims first", sessionWorkerOutputOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"output lost execution", sessionWorkerOutputOperation, lost, http.StatusConflict, "conflict", session.ErrStaleOutput.Error(), false},
		{"output stale execution", sessionWorkerOutputOperation, session.ErrStaleExecution, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"output Secret delivery", sessionWorkerOutputOperation, fmt.Errorf("lock actor output secret authority: %w", secret.ErrDeliveryUnavailable), http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"output failure", sessionWorkerOutputOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"commit claims first", sessionWorkerCommitOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"commit stale", sessionWorkerCommitOperation, errors.Join(session.ErrStaleTurnCommit, &session.OperationError{Code: "turn_unsettled"}), http.StatusConflict, "conflict", session.ErrStaleTurnCommit.Error(), false},
		{"commit rejection without stale", sessionWorkerCommitOperation, &session.OperationError{Code: "turn_unsettled"}, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"commit Secret delivery", sessionWorkerCommitOperation, fmt.Errorf("lock actor turn secret authority: %w", secret.ErrDeliveryUnavailable), http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"commit failure", sessionWorkerCommitOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"complete claims first", sessionWorkerCompleteOperation, claims, http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"complete cleanup pending", sessionWorkerCompleteOperation, errors.Join(session.ErrStopCleanupPending, session.ErrStaleCompletion), http.StatusServiceUnavailable, "service_unavailable", "service is unavailable", false},
		{"complete stale", sessionWorkerCompleteOperation, session.ErrStaleCompletion, http.StatusConflict, "conflict", session.ErrStaleCompletion.Error(), false},
		{"complete admission", sessionWorkerCompleteOperation, fmt.Errorf("%w: %v", session.ErrCompletionAdmission, secret.ErrDeliveryUnavailable), http.StatusUnprocessableEntity, "unprocessable_entity", "actor completion admission is invalid", false},
		{"complete check violation", sessionWorkerCompleteOperation, fmt.Errorf("finish: %w", &pgconn.PgError{Code: "23514"}), http.StatusUnprocessableEntity, "unprocessable_entity", "actor completion admission is invalid", false},
		{"complete Secret delivery with replay failure", sessionWorkerCompleteOperation, errors.Join(secret.ErrDeliveryUnavailable, unavailable), http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"complete failure", sessionWorkerCompleteOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"wait claims first", sessionWorkerWaitOperation, errors.Join(workergroup.ErrStaleClaims, session.ErrStaleExecution), http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"wait failure", sessionWorkerWaitOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
	}
	for _, stale := range []error{session.ErrStaleExecution, run.ErrWaitCursor, session.ErrAuthority, run.ErrTurnStopped, run.ErrTurnScope} {
		cases = append(cases, sessionErrorCase{"wait stale " + stale.Error(), sessionWorkerWaitOperation, fmt.Errorf("register: %w", stale), http.StatusConflict, "conflict", "worker actor input wait receipt is stale", false})
	}
	for _, other := range []error{run.ErrTurnNotActive, run.ErrTurnUnsettled, run.ErrStale} {
		cases = append(cases, sessionErrorCase{"wait not stale " + other.Error(), sessionWorkerWaitOperation, other, http.StatusInternalServerError, "internal_error", "internal server error", false})
	}
	assertSessionErrors(t, cases)
}

func TestSessionWorkerFailureReportsWorkerHandledRejections(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want workerapi.RuntimeOperationFailure
	}{
		{"expired", idempotency.ExpiredError{}, workerapi.RuntimeOperationFailure{Code: "operation_expired", Message: idempotency.ExpiredError{}.Error()}},
		{"session rejection", &session.OperationError{Code: "session_held"}, workerapi.RuntimeOperationFailure{Code: "session_held", Message: "session_held"}},
		{"missing Session", &session.OperationError{Code: "session_not_found"}, workerapi.RuntimeOperationFailure{Code: "session_not_found", Message: "session_not_found"}},
		{"stale execution rejection", &session.OperationError{Code: "stale_execution"}, workerapi.RuntimeOperationFailure{Code: "stale_execution", Message: "stale_execution"}},
		{"turn unsettled", fmt.Errorf("validate: %w", run.ErrTurnUnsettled), workerapi.RuntimeOperationFailure{Code: "turn_unsettled", Message: "turn_unsettled"}},
		{"turn stopping", run.ErrTurnStopped, workerapi.RuntimeOperationFailure{Code: "turn_stopping", Message: run.ErrTurnStopped.Error()}},
		{"turn not active", run.ErrTurnNotActive, workerapi.RuntimeOperationFailure{Code: "turn_not_active", Message: run.ErrTurnNotActive.Error()}},
		{"turn scope", run.ErrTurnScope, workerapi.RuntimeOperationFailure{Code: "stale_execution", Message: run.ErrTurnScope.Error()}},
		{"idempotency conflict", idempotency.ConflictError{ClaimID: uuid.NewV7()}, workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor output"}},
		{"too large", session.ErrOutputTooLarge, workerapi.RuntimeOperationFailure{Code: "actor_output_too_large", Message: session.ErrOutputTooLarge.Error()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := sessionWorkerFailure(test.err)
			if !ok || got != test.want {
				t.Fatalf("failure = %+v %v, want %+v", got, ok, test.want)
			}
		})
	}
	for _, err := range []error{
		run.ErrStaleSource, session.ErrStaleOutput, session.ErrStaleExecution, session.ErrAuthority,
		secret.ErrDeliveryUnavailable, workergroup.ErrStaleClaims, run.ErrWaitCursor, errors.New("database failed"),
	} {
		if got, ok := sessionWorkerFailure(err); ok {
			t.Fatalf("%v reported failure %+v", err, got)
		}
	}
}

func TestWriteWorkerSessionCommandOutcomes(t *testing.T) {
	lost := errors.Join(session.ErrStaleOutput, pgx.ErrNoRows)
	for _, test := range []struct {
		name    string
		err     error
		status  int
		failure string
		message string
		logged  bool
	}{
		{name: "accepted", status: http.StatusOK},
		{name: "stale source", err: run.ErrStaleSource, status: http.StatusOK, failure: "stale_execution"},
		{name: "stale source before claims", err: errors.Join(run.ErrStaleSource, workergroup.ErrStaleClaims), status: http.StatusOK, failure: "stale_execution"},
		{name: "rejection", err: &session.OperationError{Code: "turn_not_ready"}, status: http.StatusOK, failure: "turn_not_ready"},
		{name: "rejection before claims", err: errors.Join(&session.OperationError{Code: "session_held"}, workergroup.ErrStaleClaims), status: http.StatusOK, failure: "session_held"},
		{name: "turn stopping", err: run.ErrTurnStopped, status: http.StatusOK, failure: "turn_stopping"},
		{name: "turn not active", err: run.ErrTurnNotActive, status: http.StatusOK, failure: "turn_not_active"},
		{name: "turn scope", err: run.ErrTurnScope, status: http.StatusOK, failure: "stale_execution"},
		{name: "idempotency conflict", err: idempotency.ConflictError{}, status: http.StatusOK, failure: "idempotency_conflict"},
		{name: "expired", err: idempotency.ExpiredError{}, status: http.StatusOK, failure: "operation_expired"},
		{name: "claims", err: workergroup.ErrStaleClaims, status: http.StatusUnauthorized, message: "worker authentication is required"},
		{name: "lost execution", err: lost, status: http.StatusConflict, message: lost.Error()},
		{name: "stale execution", err: session.ErrStaleExecution, status: http.StatusConflict, message: session.ErrStaleExecution.Error()},
		{name: "Session authority", err: session.ErrAuthority, status: http.StatusInternalServerError, message: "internal server error", logged: true},
		{name: "target changed", err: run.ErrExecutionTargetChanged, status: http.StatusServiceUnavailable, message: "Session control target changed; retry the operation"},
		{name: "Secret revoked", err: secret.ErrDeliveryRevoked, status: http.StatusConflict, message: "secret delivery is no longer authorized"},
		{name: "Secret delivery", err: secret.ErrDeliveryUnavailable, status: http.StatusInternalServerError, message: "internal server error", logged: true},
		{name: "failure", err: errors.New("database failed"), status: http.StatusInternalServerError, message: "internal server error", logged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
			recorder := httptest.NewRecorder()
			server.writeWorkerSessionCommand(recorder, "correlation", test.err)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if logged := strings.Contains(logs.String(), `"msg":"Session worker command"`); logged != test.logged {
				t.Fatalf("logged = %v: %s", logged, logs.String())
			}
			if test.status != http.StatusOK {
				if got := decodeHTTPError(t, recorder.Body.Bytes()).Message; got != test.message {
					t.Fatalf("message = %q, want %q", got, test.message)
				}
				return
			}
			var response workerapi.TurnCommandResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.CorrelationID != "correlation" || response.Accepted != (test.err == nil) {
				t.Fatalf("response = %+v", response)
			}
			if (response.Failed == nil) != (test.failure == "") || (response.Failed != nil && (response.Failed.Code != test.failure || response.Failed.Retryable)) {
				t.Fatalf("failure = %+v, want %q", response.Failed, test.failure)
			}
		})
	}
}
