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

func (s *Server) workerAgentAsk(w http.ResponseWriter, r *http.Request, req workerapi.AgentOperationRequest, host workergroup.HostPrincipal, e session.Execution, turn uuid.UUID) {
	if _, err := canonicalJSON(req.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", "ambiguous ask arguments"))
		return
	}
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
	if err := session.RuntimeAsk(r.Context(), s.tx, host, e, turn, id, input.Question); err != nil {
		s.writeAgentAdmissionError(w, req.RequestID, err)
		return
	}
	value, _ := json.Marshal(map[string]string{"id": id.String()})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: req.RequestID, Value: value})
}

func decodeAgentAskID(payload json.RawMessage) (uuid.UUID, error) {
	if _, err := canonicalJSON(payload); err != nil {
		return uuid.Nil(), errors.New("ambiguous ask arguments")
	}
	var input struct {
		AskID string `json:"askId"`
	}
	if err := decodeAgentPayload(payload, &input); err != nil {
		return uuid.Nil(), errors.New("invalid ask arguments")
	}
	id, err := uuid.Parse(input.AskID)
	if err != nil || id == uuid.Nil() {
		return uuid.Nil(), errors.New("invalid question identity")
	}
	return id, nil
}

func (s *Server) workerAgentWithdrawAsk(w http.ResponseWriter, r *http.Request, req workerapi.AgentOperationRequest, host workergroup.HostPrincipal, e session.Execution, turn uuid.UUID) {
	id, err := decodeAgentAskID(req.Payload)
	if err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", err.Error()))
		return
	}
	if err := session.RuntimeWithdrawAsk(r.Context(), s.tx, host, e, turn, id); err != nil {
		s.writeAgentAdmissionError(w, req.RequestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: req.RequestID, Value: json.RawMessage(`null`)})
}

func (s *Server) workerAgentWaitAsk(w http.ResponseWriter, r *http.Request, req workerapi.AgentOperationRequest, host workergroup.HostPrincipal, e session.Execution, turn uuid.UUID) {
	id, err := decodeAgentAskID(req.Payload)
	if err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(req.RequestID, "invalid_arguments", err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// A pending observation releases the host's shared operation slot. The SDK
	// polls the same durable ask; waiting here can starve withdrawal and drainage.
	view, err := session.RuntimeObserveAsk(ctx, s.tx, host, e, turn, id)
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
