package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerInterruptSessionTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.InterruptSessionTurnRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn interrupt JSON: %w", err))
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
	var receipt session.InterruptReceipt
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		receipt, err = session.InterruptFromRun(r.Context(), s.tx, fence, session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{SessionID: pgvalue.MustUUIDValue(sessionID)}, IdempotencyKey: request.IdempotencyKey}, TurnID: turnID})
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.InterruptSessionTurnResponse{CorrelationID: request.CorrelationID, Completed: &api.TurnInterruptReceipt{ID: receipt.ID.String(), SessionID: pgvalue.UUIDString(sessionID), TurnID: turnID.String(), HoldID: receipt.HoldID.String(), Status: receipt.Status}})
}

func (s *Server) workerResumeSession(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ResumeSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session resume JSON: %w", err))
		return
	}
	sessionID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	holdID, err := parseCanonicalUUID("hold_id", request.HoldID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var receipt session.ControlReceipt
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		receipt, err = session.ResumeFromRun(r.Context(), s.tx, fence, session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{SessionID: pgvalue.MustUUIDValue(sessionID)}, IdempotencyKey: request.IdempotencyKey}, HoldID: holdID})
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ResumeSessionResponse{CorrelationID: request.CorrelationID, Completed: &api.SessionResumeReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), HoldID: holdID.String(), Status: receipt.Status}})
}
