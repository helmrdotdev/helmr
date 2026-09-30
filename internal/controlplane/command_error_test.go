package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestCommandErrorMapsWorkerOperations(t *testing.T) {
	invalidCompletion := fmt.Errorf("%w: exited command requires exit_code and no error", command.ErrInvalidCompletion)
	for _, test := range []struct {
		name      string
		operation commandOperation
		err       error
		status    int
		message   string
	}{
		{"claim stale claims", commandClaimOperation, workergroup.ErrStaleClaims, http.StatusUnauthorized, "worker authentication is required"},
		{"claim stale claims first", commandClaimOperation, errors.Join(command.ErrChanged, workergroup.ErrStaleClaims), http.StatusUnauthorized, "worker authentication is required"},
		{"claim changed", commandClaimOperation, command.ErrChanged, http.StatusConflict, "command claim is stale"},
		{"claim invalid completion", commandClaimOperation, invalidCompletion, http.StatusInternalServerError, "internal server error"},
		{"claim unmapped", commandClaimOperation, errors.New("database is down"), http.StatusInternalServerError, "internal server error"},
		{"completion stale claims", commandCompletionOperation, workergroup.ErrStaleClaims, http.StatusUnauthorized, "worker authentication is required"},
		{"completion changed", commandCompletionOperation, command.ErrChanged, http.StatusConflict, "command completion is stale or differs from its receipt"},
		{"completion invalid", commandCompletionOperation, invalidCompletion, http.StatusBadRequest, invalidCompletion.Error()},
		{"completion unmapped", commandCompletionOperation, errors.New("database is down"), http.StatusInternalServerError, "internal server error"},
		{"log stale claims", commandLogAppendOperation, workergroup.ErrStaleClaims, http.StatusUnauthorized, "worker authentication is required"},
		{"log changed", commandLogAppendOperation, command.ErrChanged, http.StatusConflict, "command log producer is stale or sequence contains different content"},
		{"log invalid", commandLogAppendOperation, command.ErrInvalidLog, http.StatusBadRequest, command.ErrInvalidLog.Error()},
		{"log invalid completion", commandLogAppendOperation, invalidCompletion, http.StatusServiceUnavailable, "service is unavailable"},
		{"log unmapped", commandLogAppendOperation, errors.New("database is down"), http.StatusServiceUnavailable, "service is unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, commandError(test.err, test.operation))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Message != test.message {
				t.Fatalf("message = %q, want %q", body.Error.Message, test.message)
			}
		})
	}
}

func TestCommandErrorMapsPublicOperations(t *testing.T) {
	stdin := command.InputError{Kind: command.InputStdinTooLarge}
	large := command.InputError{Kind: command.InputTooLarge}
	invalid := command.InputError{Kind: command.InputInvalid}
	for _, test := range []struct {
		name      string
		operation commandOperation
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"create invalid", commandCreateOperation, invalid, http.StatusBadRequest, "invalid_computer_command", false},
		{"create too large", commandCreateOperation, large, http.StatusRequestEntityTooLarge, "computer_command_request_too_large", false},
		{"create stdin", commandCreateOperation, fmt.Errorf("wrapped: %w", stdin), http.StatusRequestEntityTooLarge, "computer_stdin_too_large", false},
		{"create computer missing", commandCreateOperation, computer.ErrNotFound, http.StatusNotFound, "computer_not_found", false},
		{"create secret", commandCreateOperation, computer.ErrSecretUnavailable, http.StatusConflict, "secret_unavailable", false},
		{"create busy", commandCreateOperation, computer.ErrBusy, http.StatusConflict, "computer_busy", true},
		{"create recovery", commandCreateOperation, computer.ErrRecoveryRequired, http.StatusConflict, "computer_recovery_required", false},
		{"create deleting", commandCreateOperation, computer.ErrDeleting, http.StatusConflict, "computer_deleting", false},
		{"create exhausted", commandCreateOperation, computer.ErrPreparationExhausted, http.StatusConflict, "computer_preparation_exhausted", false},
		{"create idempotency conflict", commandCreateOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", false},
		{"create expired", commandCreateOperation, fmt.Errorf("wrapped: %w", idempotency.ExpiredError{}), http.StatusGone, "operation_expired", false},
		{"create receipt", commandCreateOperation, command.ErrReceiptInvalid, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"create unmapped", commandCreateOperation, errors.New("database is down"), http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"get missing", commandGetOperation, command.ErrNotFound, http.StatusNotFound, "computer_command_not_found", false},
		{"get pruned", commandGetOperation, gone(codedError{code: "command_result_expired", message: "exec result has expired"}), http.StatusGone, "command_result_expired", false},
		{"get computer error", commandGetOperation, computer.ErrBusy, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"get unmapped", commandGetOperation, errors.New("exec state is invalid"), http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"cancel missing", commandCancelOperation, command.ErrNotFound, http.StatusNotFound, "computer_command_not_found", false},
		{"cancel idempotency conflict", commandCancelOperation, idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", false},
		{"cancel expired", commandCancelOperation, idempotency.ExpiredError{}, http.StatusGone, "operation_expired", false},
		{"cancel receipt", commandCancelOperation, command.ErrReceiptInvalid, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
		{"cancel input", commandCancelOperation, invalid, http.StatusServiceUnavailable, "computer_authority_unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapped := commandError(test.err, test.operation)
			recorder := httptest.NewRecorder()
			writeError(recorder, mapped)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			if code := decodeHTTPError(t, recorder.Body.Bytes()).Code; code != test.code {
				t.Fatalf("code = %s, want %s", code, test.code)
			}
			var retryer interface{ ErrorRetryable() bool }
			if retryable := errors.As(mapped, &retryer) && retryer.ErrorRetryable(); retryable != test.retryable {
				t.Fatalf("retryable = %v, want %v", retryable, test.retryable)
			}
		})
	}
}
