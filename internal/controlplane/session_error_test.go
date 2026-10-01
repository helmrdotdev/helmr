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

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type sessionErrorCase struct {
	name      string
	operation sessionOperation
	err       error
	status    int
	code      string
	message   string
	retryable bool
}

func TestSessionErrorMapsActorStart(t *testing.T) {
	unavailable := errors.New("database is down")
	assertSessionErrors(t, []sessionErrorCase{
		{"start expired", sessionStartOperation, fmt.Errorf("start: %w", idempotency.ExpiredError{}), http.StatusGone, "operation_expired", idempotency.ExpiredError{}.Error(), false},
		{"start preparation exhausted", sessionStartOperation, computer.ErrPreparationExhausted, http.StatusConflict, "computer_preparation_exhausted", "Computer preparation limit reached", false},
		{"start idempotency conflict", sessionStartOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier actor start", false},
		{"start key conflict", sessionStartOperation, session.KeyConflictError{Key: "thread:1"}, http.StatusConflict, "actor_key_conflict", `actor key "thread:1" already belongs to another actor`, false},
		{"start not deployed", sessionStartOperation, session.ErrActorNotDeployed, http.StatusNotFound, "actor_not_deployed", "actor declaration is not deployed", false},
		{"start computer not found", sessionStartOperation, session.ErrStartComputerNotFound, http.StatusNotFound, "computer_not_found", "actor start computer was not found", false},
		{"start computer unavailable", sessionStartOperation, session.ErrStartComputerUnavailable, http.StatusConflict, "computer_unavailable", "actor start computer cannot accept execution", true},
		{"start secret unavailable", sessionStartOperation, session.ErrStartSecretUnavailable, http.StatusConflict, "secret_unavailable", "actor start computer secret is unavailable", false},
		{"start invalid", sessionStartOperation, fmt.Errorf("%w: bad duration", session.ErrStartInvalid), http.StatusBadRequest, "invalid_actor_start", "actor start request is invalid: bad duration", false},
		{"start authority", sessionStartOperation, fmt.Errorf("%w: manifest", session.ErrStartAuthority), http.StatusServiceUnavailable, "actor_start_authority_unavailable", "actor start authority is unavailable", true},
		{"start receipt", sessionStartOperation, session.ErrStartReceiptInvalid, http.StatusServiceUnavailable, "actor_start_authority_unavailable", "actor start authority is unavailable", true},
		{"start failure", sessionStartOperation, unavailable, http.StatusServiceUnavailable, "actor_start_authority_unavailable", "actor start authority is unavailable", true},
		{"worker start claims first", sessionWorkerStartOperation, errors.Join(run.ErrStaleSource, workergroup.ErrStaleClaims), http.StatusUnauthorized, "unauthorized", "worker authentication is required", false},
		{"worker start stale source", sessionWorkerStartOperation, run.ErrStaleSource, http.StatusConflict, "conflict", run.ErrStaleSource.Error(), false},
		{"worker start preparation exhausted", sessionWorkerStartOperation, computer.ErrPreparationExhausted, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"worker start authority", sessionWorkerStartOperation, session.ErrStartAuthority, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"worker start failure", sessionWorkerStartOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
	})
}

func TestSessionErrorMapsPublicOperationsAndReads(t *testing.T) {
	unavailable := errors.New("database is down")
	transport := forbidden(codedError{code: "forbidden", message: "permission is required"})
	assertSessionErrors(t, []sessionErrorCase{
		{"expired", sessionPublicOperation, fmt.Errorf("send: %w", idempotency.ExpiredError{}), http.StatusGone, "operation_expired", idempotency.ExpiredError{}.Error(), false},
		{"transport", sessionPublicOperation, transport, http.StatusForbidden, "forbidden", "permission is required", false},
		{"transport bad request", sessionPublicOperation, badRequest(codedError{code: "invalid_request", message: "bad id"}), http.StatusBadRequest, "invalid_request", "bad id", false},
		{"idempotency conflict", sessionPublicOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier operation", false},
		{"session not found", sessionPublicOperation, &session.OperationError{Code: "session_not_found"}, http.StatusNotFound, "session_not_found", "session_not_found", false},
		{"turn not found", sessionPublicOperation, &session.OperationError{Code: "turn_not_found"}, http.StatusNotFound, "turn_not_found", "turn_not_found", false},
		{"invalid request", sessionPublicOperation, &session.OperationError{Code: "invalid_request"}, http.StatusBadRequest, "invalid_request", "invalid_request", false},
		{"invalid cursor", sessionPublicOperation, &session.OperationError{Code: "invalid_cursor"}, http.StatusBadRequest, "invalid_cursor", "invalid_cursor", false},
		{"forbidden", sessionPublicOperation, &session.OperationError{Code: "forbidden"}, http.StatusForbidden, "forbidden", "forbidden", false},
		{"cursor expired", sessionPublicOperation, &session.OperationError{Code: "cursor_expired", RetainedAfter: 17}, http.StatusGone, "cursor_expired", "cursor_expired", false},
		{"session held", sessionPublicOperation, &session.OperationError{Code: "session_held"}, http.StatusConflict, "session_held", "session_held", false},
		{"session not open", sessionPublicOperation, &session.OperationError{Code: "session_not_open"}, http.StatusConflict, "session_not_open", "session_not_open", false},
		{"no rows", sessionPublicOperation, fmt.Errorf("lock: %w", pgx.ErrNoRows), http.StatusNotFound, "session_not_found", "Session not found", false},
		{"computer authority", sessionPublicOperation, fmt.Errorf("%w: %w", session.ErrComputerAuthority, computer.ErrNotFound), http.StatusNotFound, "session_not_found", "Session not found", false},
		{"failure", sessionPublicOperation, unavailable, http.StatusInternalServerError, "internal_error", "internal server error", false},
		{"get not found", sessionGetOperation, session.ErrNotFound, http.StatusNotFound, "session_not_found", "session was not found", false},
		{"get no rows is unavailable", sessionGetOperation, pgx.ErrNoRows, http.StatusServiceUnavailable, "session_authority_unavailable", "Session authority is unavailable", true},
		{"get failure", sessionGetOperation, unavailable, http.StatusServiceUnavailable, "session_authority_unavailable", "Session authority is unavailable", true},
		{"list failure", sessionListOperation, unavailable, http.StatusServiceUnavailable, "session_authority_unavailable", "Session authority is unavailable", true},
		{"list not found is unavailable", sessionListOperation, session.ErrNotFound, http.StatusServiceUnavailable, "session_authority_unavailable", "Session authority is unavailable", true},
	})
	recorder := httptest.NewRecorder()
	writeError(recorder, sessionError(&session.OperationError{Code: "cursor_expired", RetainedAfter: 17}, sessionPublicOperation))
	if body := decodeHTTPError(t, recorder.Body.Bytes()); string(body.Details["retained_after"]) != "17" {
		t.Fatalf("cursor expiry details = %s", recorder.Body.String())
	}
}

func TestWriteSessionErrorLogsInternalAndUnavailableCauses(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	server.writeSessionError(httptest.NewRecorder(), &session.OperationError{Code: "session_held"}, sessionPublicOperation)
	server.writeSessionError(httptest.NewRecorder(), session.ErrNotFound, sessionGetOperation)
	server.writeSessionError(httptest.NewRecorder(), session.ErrStartAuthority, sessionStartOperation)
	if logs.Len() != 0 {
		t.Fatalf("handled outcomes logged: %s", logs.String())
	}
	server.writeSessionError(httptest.NewRecorder(), errors.New("public-sentinel"), sessionPublicOperation)
	server.writeSessionError(httptest.NewRecorder(), errors.New("read-sentinel"), sessionListOperation)
	for _, want := range []string{`"msg":"session operation failed"`, `public-sentinel`, `"msg":"read Session failed"`, `read-sentinel`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %s: %s", want, logs.String())
		}
	}
	(&Server{}).writeSessionError(httptest.NewRecorder(), errors.New("unlogged"), sessionPublicOperation)
}

func assertSessionErrors(t *testing.T, cases []sessionErrorCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mapped := sessionError(test.err, test.operation)
			var retryer errorRetryer
			retryable := errors.As(mapped, &retryer) && retryer.ErrorRetryable()
			recorder := httptest.NewRecorder()
			writeError(recorder, mapped)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || body.Error.Message != test.message || retryable != test.retryable {
				t.Fatalf("error = %s %q retryable=%v, want %s %q retryable=%v", body.Error.Code, body.Error.Message, retryable, test.code, test.message, test.retryable)
			}
		})
	}
}

func TestActorStartFailureReportsWorkerHandledRejections(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want workerapi.RuntimeOperationFailure
	}{
		{"session rejection", &session.OperationError{Code: "session_held"}, workerapi.RuntimeOperationFailure{Code: "session_held", Message: "session_held"}},
		{"expired", idempotency.ExpiredError{}, workerapi.RuntimeOperationFailure{Code: "operation_expired", Message: idempotency.ExpiredError{}.Error()}},
		{"idempotency conflict", idempotency.ConflictError{}, workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor start"}},
		{"key conflict", session.KeyConflictError{Key: "k"}, workerapi.RuntimeOperationFailure{Code: "actor_key_conflict", Message: `actor key "k" already belongs to another actor`}},
		{"not deployed", session.ErrActorNotDeployed, workerapi.RuntimeOperationFailure{Code: "actor_not_deployed", Message: "actor declaration is not deployed"}},
		{"computer not found", session.ErrStartComputerNotFound, workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: "actor start computer was not found"}},
		{"computer unavailable", session.ErrStartComputerUnavailable, workerapi.RuntimeOperationFailure{Code: "computer_unavailable", Message: "actor start computer cannot accept execution", Retryable: true}},
		{"secret unavailable", session.ErrStartSecretUnavailable, workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: "actor start computer secret is unavailable"}},
		{"invalid", fmt.Errorf("%w: key", session.ErrStartInvalid), workerapi.RuntimeOperationFailure{Code: "invalid_actor_start", Message: "actor start request is invalid: key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := actorStartFailure(test.err)
			if !ok || got != test.want {
				t.Fatalf("failure = %+v %v, want %+v", got, ok, test.want)
			}
		})
	}
	for _, err := range []error{computer.ErrPreparationExhausted, session.ErrStartAuthority, session.ErrStartReceiptInvalid, run.ErrStaleSource, errors.New("database is down")} {
		if got, ok := actorStartFailure(err); ok {
			t.Fatalf("%v reported failure %+v", err, got)
		}
	}
}

func TestWriteWorkerActorStartErrorKeepsStaleSourceOutOfFailures(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	stale := errors.Join(run.ErrStaleSource, &session.OperationError{Code: "session_held"})
	recorder := httptest.NewRecorder()
	server.writeWorkerActorStartError(recorder, "correlation", "lease", stale)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale source = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.writeWorkerActorStartError(recorder, "correlation", "lease", session.ErrStartComputerNotFound)
	var response workerapi.StartActorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.CorrelationID != "correlation" || response.Failed == nil || response.Failed.Code != "computer_not_found" {
		t.Fatalf("failure = %d %s", recorder.Code, recorder.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("handled outcomes logged: %s", logs.String())
	}
	server.writeWorkerActorStartError(httptest.NewRecorder(), "correlation", "lease", errors.New("credential=sentinel"))
	if !strings.Contains(logs.String(), `"msg":"start run-sourced Actor"`) || !strings.Contains(logs.String(), `"run_lease_id":"lease"`) {
		t.Fatalf("internal failure logs = %s", logs.String())
	}
}
