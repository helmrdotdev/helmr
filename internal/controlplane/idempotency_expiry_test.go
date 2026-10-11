package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/idempotency"
)

func TestPrunedOperationReceiptHasExplicitTransportOutcome(t *testing.T) {
	server := &Server{}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"computer exec": func(w http.ResponseWriter, err error) {
			server.writeCommandError(w, err, commandCreateOperation)
		},
		"computer create": func(w http.ResponseWriter, err error) {
			server.writeComputerError(w, err, computerCreateOperation, "create Computer failed")
		},
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
}
