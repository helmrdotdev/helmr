package controlplane

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
)

func (s *Server) sendTurnMessageHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsSend)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	turn, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	var request api.SessionMessageRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	req := session.EnqueueRequest{EnvironmentID: environment, SessionID: sessionID, IdempotencyKey: request.IdempotencyKey, Input: request.Message}
	receipt, err := session.SendTurn(r.Context(), s.auditPolicy, s.tx, caller, req, turn)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionMessageReceipt{ID: receipt.MessageID.String(), TurnID: receipt.TurnID.String(), MessageID: receipt.MessageID.String(), Status: "accepted"})
}
