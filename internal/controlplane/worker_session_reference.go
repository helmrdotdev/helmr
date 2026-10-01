package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerGetSessionTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnReferenceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn retrieve JSON: %w", err))
		return
	}
	sessionID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var result api.SessionTurn
	view, err := session.GetTurnFromRun(r.Context(), s.tx, workerSourceReceipt(workerFromContext(r.Context()), request.Lease), pgvalue.MustUUIDValue(sessionID), turnID)
	if err == nil {
		result, err = projectSessionTurn(view)
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.SessionTurnResponse{CorrelationID: request.CorrelationID, Completed: &result})
}
