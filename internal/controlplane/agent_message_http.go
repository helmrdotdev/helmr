package controlplane

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"net/http"
	"uuid"
)

func (s *Server) sendSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsSend)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request struct {
		Data           json.RawMessage `json:"data"`
		IdempotencyKey string          `json:"idempotency_key,omitempty"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	req := agent.EnqueueRequest{EnvironmentID: environment, SessionID: session, RetryKey: request.IdempotencyKey, Input: request.Data}
	var receipt agent.SendReceipt
	exact := chi.URLParam(r, "turnID")
	if exact != "" {
		turn, err := ids.Parse(exact)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
		receipt, err = agent.SendTurn(r.Context(), s.tx, caller, req, turn)
		if err != nil {
			s.writeAgentHTTPError(w, err)
			return
		}
	} else {
		receipt, err = agent.Send(r.Context(), s.tx, caller, req)
		if err != nil {
			s.writeAgentHTTPError(w, err)
			return
		}
	}
	if receipt.MessageID == uuid.Nil() {
		writeJSON(w, http.StatusOK, map[string]any{"id": receipt.TurnID.String(), "kind": "enqueued", "turn_id": receipt.TurnID.String()})
		return
	}
	response := map[string]any{"id": receipt.MessageID.String(), "kind": "messaged", "turn_id": receipt.TurnID.String(), "message_id": receipt.MessageID.String()}
	if exact != "" {
		delete(response, "kind")
		response["status"] = "accepted"
	}
	writeJSON(w, http.StatusOK, response)
}
