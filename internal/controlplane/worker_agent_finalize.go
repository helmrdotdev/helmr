package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (server *Server) workerAgentFinalize(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, host workergroup.HostPrincipal, execution session.Execution, turn uuid.UUID) {
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
	outcome, err := session.RuntimeFinalize(ctx, server.auditPolicy, server.tx, host, execution, turn, input.Result, request.DrainEvidence)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if ctx.Err() != nil {
			writeError(w, unavailable(errors.New("turn finalization deadline elapsed")))
			return
		}
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	if outcome == nil {
		writeError(w, unavailable(codedError{code: workerapi.TurnReceiptsPending, message: "turn callback receipts pending", retryable: true}))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: outcome})
}
