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
