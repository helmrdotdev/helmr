package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (server *Server) workerAgentFinalize(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, host workergroup.HostPrincipal, execution agent.Execution, turn uuid.UUID) {
	var input struct {
		Result json.RawMessage `json:"result"`
	}
	if _, err := canonicalJSON(request.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous finalization arguments"))
		return
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil || !json.Valid(input.Result) || request.DrainEvidence == "" || len(request.DrainEvidence) > 512 {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "finalization requires a result and native drainage evidence"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		outcome, err := agent.RuntimeFinalize(ctx, server.tx, host, execution, turn, input.Result, request.DrainEvidence)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if ctx.Err() != nil {
				writeError(w, unavailable(errors.New("turn save is still pending")))
				return
			}
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		if outcome != nil {
			writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: outcome})
			return
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return
			}
			// Keep the retained operation unresolved. The independent save owner
			// progresses the same cut; retry must not turn pending into failure.
			writeError(w, unavailable(errors.New("turn save is still pending")))
			return
		case <-ticker.C:
		}
	}
}
