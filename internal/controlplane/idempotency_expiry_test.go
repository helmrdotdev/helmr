package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPrunedOperationReceiptHasExplicitTransportOutcome(t *testing.T) {
	server := &Server{}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"computer exec": server.writeComputerCommandError,
		"computer create": func(w http.ResponseWriter, err error) {
			server.writeComputerError(w, err, computerCreateOperation, "create Computer failed")
		},
		"actor start": server.writeActorStartError,
		"task start":  server.writeTaskStartError,
		"session":     server.writeSessionOperationError,
		"token":       server.writeTokenError,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			write(recorder, fmt.Errorf("operation: %w", idempotency.ExpiredError{}))
			if recorder.Code != http.StatusGone {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
			}
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "operation_expired" || body.Error.Retryable {
				t.Fatalf("body=%s", recorder.Body)
			}
		})
	}
	for name, convert := range map[string]func(error) (workerapi.RuntimeOperationFailure, bool){
		"computer create": func(err error) (workerapi.RuntimeOperationFailure, bool) {
			return workerComputerFailure(err, computerCreateOperation)
		},
		"computer delete": func(err error) (workerapi.RuntimeOperationFailure, bool) {
			return workerComputerFailure(err, computerDeleteOperation)
		},
		"actor start": workerActorStartFailure, "actor output": actorOutputAppendFailure,
	} {
		t.Run(name+" worker", func(t *testing.T) {
			result, ok := convert(idempotency.ExpiredError{})
			if !ok || result.Code != "operation_expired" || result.Retryable {
				t.Fatalf("failure=%+v mapped=%v", result, ok)
			}
		})
	}
}
