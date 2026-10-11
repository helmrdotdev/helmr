package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/agent"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"net/http"
	"time"
	"uuid"
)

func (s *Server) workerAgentAsk(w http.ResponseWriter, r *http.Request, req workerapi.AgentOperationRequest, host workergroup.HostPrincipal, e agent.Execution, turn uuid.UUID) {
	if _, err := canonicalJSON(req.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "ambiguous ask arguments"))
		return
	}
	if agentv1.Operation_Method(req.Method) == agentv1.Operation_METHOD_ASK {
		var input struct {
			CreationID string          `json:"creationId"`
			Question   json.RawMessage `json:"question"`
		}
		if err := decodeAgentPayload(req.Payload, &input); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "invalid question arguments"))
			return
		}
		id, err := uuid.Parse(input.CreationID)
		if err != nil || id == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "invalid question identity"))
			return
		}
		if err := agent.RuntimeAsk(r.Context(), s.tx, host, e, turn, id, input.Question); err != nil {
			s.writeAgentAdmissionError(w, req.RequestID, err)
			return
		}
		value, _ := json.Marshal(map[string]string{"id": id.String()})
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: req.RequestID, Value: value})
		return
	}
	var input struct {
		AskID string `json:"askId"`
	}
	if err := decodeAgentPayload(req.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "invalid ask arguments"))
		return
	}
	id, err := uuid.Parse(input.AskID)
	if err != nil || id == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "invalid question identity"))
		return
	}
	if agentv1.Operation_Method(req.Method) == agentv1.Operation_METHOD_WITHDRAW_ASK {
		if err := agent.RuntimeWithdrawAsk(r.Context(), s.tx, host, e, turn, id); err != nil {
			s.writeAgentAdmissionError(w, req.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: req.RequestID, Value: json.RawMessage(`null`)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// A pending observation releases the host's shared operation slot. The SDK
	// polls the same durable ask; waiting here can starve withdrawal and drainage.
	view, err := agent.RuntimeObserveAsk(ctx, s.tx, host, e, turn, id)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if ctx.Err() != nil {
			writeError(w, unavailable(errors.New("ask observation is temporarily unavailable")))
			return
		}
		s.writeAgentAdmissionError(w, req.RequestID, err)
		return
	}
	if view.Status == "cancelled" {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "ask_cancelled", "Ask was cancelled"))
		return
	}
	value := json.RawMessage(`null`)
	if view.Status == "responded" {
		if view.PayloadExpiredAt != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "payload_expired", "Ask payload has expired"))
			return
		}
		kind, responder := "user", view.RespondedByUserID
		if responder == nil {
			kind, responder = "api_key", view.RespondedByAPIKeyID
		}
		value, _ = json.Marshal(map[string]any{"answer": view.Answer, "respondedBy": map[string]string{"kind": kind, "id": responder.String()}})
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: req.RequestID, Value: value})
}
