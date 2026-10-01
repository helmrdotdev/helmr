package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerSendSession(w http.ResponseWriter, r *http.Request) {
	s.workerAdmitSession(w, r, session.SendMessageOrEnqueue)
}
func (s *Server) workerEnqueueSession(w http.ResponseWriter, r *http.Request) {
	s.workerAdmitSession(w, r, session.EnqueueOnly)
}
func (s *Server) workerSendTurnMessage(w http.ResponseWriter, r *http.Request) {
	s.workerAdmitSession(w, r, session.ExactMessage)
}
func (s *Server) workerAdmitSession(w http.ResponseWriter, r *http.Request, mode session.AdmissionMode) {
	var request workerapi.SubmitSessionDataRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session submission JSON: %w", err))
		return
	}
	targetID, err := ids.Parse(request.SessionID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = api.ValidateSessionDataRequest(api.SessionDataRequest{Data: request.Data, IdempotencyKey: request.IdempotencyKey}); err != nil {
		writeError(w, badRequest(err))
		return
	}
	command := session.AdmissionRequest{Mode: mode, Data: request.Data, IdempotencyKey: request.IdempotencyKey}
	if mode == session.ExactMessage {
		if request.TurnID == nil {
			writeError(w, badRequest(errors.New("turn_id is required")))
			return
		}
		command.TurnID, err = ids.Parse(*request.TurnID)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	} else if request.TurnID != nil {
		writeError(w, badRequest(errors.New("turn_id is not accepted on Session admission")))
		return
	}
	var receipt session.AdmissionReceipt
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		command.SessionID = targetID
		receipt, err = session.AdmitFromRun(r.Context(), s.tx, fence, command)
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := api.SessionAdmissionReceipt{ID: receipt.ID.String(), Kind: receipt.Kind, TurnID: receipt.TurnID.String()}
	if receipt.MessageID != nil {
		id := receipt.MessageID.String()
		response.MessageID = &id
	}
	writeJSON(w, http.StatusOK, workerapi.SubmitSessionDataResponse{CorrelationID: request.CorrelationID, Completed: &response})
}
