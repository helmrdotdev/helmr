package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestComputerContinuationCapacityRejectionIsDefinitive(t *testing.T) {
	for _, rejection := range []error{workergroup.ErrCapacityUnavailable, workergroup.ErrCapacityExhausted, workergroup.ErrCapacityIncompatible, computer.ErrExecutionLimit} {
		t.Run(rejection.Error(), func(t *testing.T) {
			response := httptest.NewRecorder()
			(&Server{}).writeAgentComputerError(response, rejection)
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusConflict || body.Error.Code != workerapi.AgentComputerNotReady {
				t.Fatalf("capacity rejection became uncertain: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
