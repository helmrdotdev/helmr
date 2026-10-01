package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The Turn and wait-cursor sentinels come from run; each worker boundary keeps
// the outcome it gave the equivalent session error.
func TestRunTurnSentinelBoundaries(t *testing.T) {
	sentinels := []error{run.ErrTurnScope, run.ErrTurnNotActive, run.ErrTurnStopped, run.ErrTurnUnsettled, run.ErrWaitCursor}
	unrelated := errors.New("database failed")
	for _, test := range []struct {
		boundary string
		err      error
		// failure is the 200 failure code, or "" when the error is not a
		// semantic failure.
		failure string
		// message is the exact 200 failure message where it is fixed.
		message                string
		staleTimer, staleInput bool
	}{
		{"scope", run.ErrTurnScope, "stale_execution", "", true, true},
		{"not active", run.ErrTurnNotActive, "turn_not_active", "", false, false},
		{"stopped", run.ErrTurnStopped, "turn_stopping", "", true, true},
		{"unsettled", run.ErrTurnUnsettled, "turn_unsettled", "turn_unsettled", false, false},
		{"wait cursor", run.ErrWaitCursor, "", "", true, true},
		{"Session input authority", session.ErrAuthority, "", "", false, true},
		{"stale execution", session.ErrStaleExecution, "", "", false, true},
		{"stale run lease", run.ErrStale, "", "", true, false},
		{"unrelated", unrelated, "", "", false, false},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			for _, err := range []error{test.err, fmt.Errorf("wrapped: %w", test.err)} {
				failure, ok := sessionWorkerFailure(err)
				if ok != (test.failure != "") || failure.Code != test.failure || failure.Retryable {
					t.Fatalf("worker Session failure(%v) = %+v, %v", err, failure, ok)
				}
				if test.message != "" && failure.Message != test.message {
					t.Fatalf("worker Session failure(%v) message = %q, want %q", err, failure.Message, test.message)
				}
				if got := errorStatus(runError(err, runTimerWaitOperation)) == http.StatusConflict; got != test.staleTimer {
					t.Fatalf("timer wait stale(%v) = %v", err, got)
				}
				if got := errorStatus(sessionError(err, sessionWorkerWaitOperation)) == http.StatusConflict; got != test.staleInput {
					t.Fatalf("Actor input wait stale(%v) = %v", err, got)
				}
			}
		})
	}
	for _, sentinel := range sentinels {
		for _, other := range sentinels {
			if sentinel != other && errors.Is(sentinel, other) {
				t.Fatalf("%v matches %v", sentinel, other)
			}
		}
	}
}

// A child invoked while its Turn is settling fails with the same 200 body the
// session operation code produced: code and message are both turn_unsettled.
func TestChildInvokeDuringSettlementKeepsOperationFailure(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, err := range []error{&session.OperationError{Code: "turn_unsettled"}, run.ErrTurnUnsettled, fmt.Errorf("validate child Turn: %w", run.ErrTurnUnsettled)} {
		response := httptest.NewRecorder()
		server.writeChildTaskInvokeError(response, "correlation", err)
		var body workerapi.InvokeChildTaskResponse
		if decodeErr := json.Unmarshal(response.Body.Bytes(), &body); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if response.Code != http.StatusOK || body.CorrelationID != "correlation" || body.Failed == nil || *body.Failed != (workerapi.RuntimeOperationFailure{Code: "turn_unsettled", Message: "turn_unsettled"}) {
			t.Fatalf("%v: status=%d body=%s", err, response.Code, response.Body.String())
		}
	}
}
