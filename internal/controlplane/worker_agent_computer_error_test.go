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
	for rejection, code := range map[error]string{
		workergroup.ErrCapacityUnavailable:  workerapi.AgentComputerNotReady,
		workergroup.ErrCapacityExhausted:    workerapi.AgentComputerNotReady,
		workergroup.ErrCapacityIncompatible: workerapi.AgentComputerNotReady,
		computer.ErrExecutionLimit:          workerapi.AgentComputerNotReady,
		computer.ErrRetainedHostCapacity:    workerapi.AgentComputerSourceCapacityAssigned,
	} {
		t.Run(rejection.Error(), func(t *testing.T) {
			response := httptest.NewRecorder()
			(&Server{}).writeAgentComputerError(response, httptest.NewRequest(http.MethodPost, "/worker/v1/agent-computers/capture/begin", nil), rejection)
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusConflict || body.Error.Code != code {
				t.Fatalf("capacity rejection became uncertain: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
