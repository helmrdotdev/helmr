package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerGetSessionTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnReferenceRequest
	if err := decodeWorkerActorRequest(r, &request, "Turn retrieve"); err != nil {
		writeError(w, badRequest(err))
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		view, err := session.GetTurn(r.Context(), work.q, session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(sessionID)}, turnID)
		if err != nil {
			return err
		}
		result, err = projectSessionTurn(view)
		return err
	})
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.SessionTurnResponse{CorrelationID: request.CorrelationID, Completed: &result})
}
