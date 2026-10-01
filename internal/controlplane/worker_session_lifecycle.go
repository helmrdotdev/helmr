package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

// workerTurnWork addresses the request's Turn work. A turn_id that is not
// a canonical UUIDv7 is uuid.Nil, which the session owner rejects after it
// locks the execution.
func workerTurnWork(request workerapi.TurnExecutionRequest) session.TurnWork {
	turnID, _ := parseCanonicalUUID("turn_id", request.TurnID)
	return session.TurnWork{TurnID: turnID, RunGeneration: request.RunGeneration}
}

func projectWorkerSessionEvent(event db.SessionEvent, deploymentID pgtype.UUID) api.SessionEvent {
	result := api.SessionEvent{ID: pgvalue.UUIDString(event.ID), SessionID: pgvalue.UUIDString(event.SessionID), Sequence: event.Sequence, CreatedAt: event.CreatedAt.Time.UTC(), Kind: event.Kind, Data: event.Data}
	if event.TurnID.Valid {
		id := pgvalue.UUIDString(event.TurnID)
		result.TurnID = &id
	}
	if event.ProducerRunID.Valid {
		result.Provenance = &api.SessionEventProvenance{RunID: pgvalue.UUIDString(event.ProducerRunID), AttemptNumber: event.ProducerAttemptNumber.Int32, RunGeneration: event.RunGeneration.Int64, DeploymentID: pgvalue.UUIDString(deploymentID)}
	}
	return result
}
func (s *Server) workerTurnMessagesReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnExecutionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn readiness JSON: %w", err))
		return
	}
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		err = session.DeclareMessageReadyFromRun(r.Context(), s.tx, fence, workerTurnWork(request))
	}
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerBeginTurnSettlement(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnExecutionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn settlement JSON: %w", err))
		return
	}
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		err = session.BeginSettlementFromRun(r.Context(), s.tx, fence, workerTurnWork(request))
	}
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerClaimTurnMessage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ClaimTurnMessageRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid message claim JSON: %w", err))
		return
	}
	deliveryID, err := parseCanonicalUUID("delivery_id", request.DeliveryID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var message db.SessionMessage
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		message, err = session.ClaimMessageFromRun(r.Context(), s.tx, fence, workerTurnWork(request.TurnExecutionRequest), deliveryID)
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := workerapi.ClaimTurnMessageResponse{CorrelationID: request.CorrelationID}
	if message.ID.Valid {
		response.Delivery = &workerapi.TurnMessageDelivery{MessageID: pgvalue.UUIDString(message.ID), TurnID: pgvalue.UUIDString(message.TurnID), DeliveryID: pgvalue.UUIDString(message.DeliveryID), Sequence: message.AcceptedSequence, Data: message.Data}
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) workerCompleteTurnMessage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CompleteTurnMessageRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid message completion JSON: %w", err))
		return
	}
	messageID, err := parseCanonicalUUID("message_id", request.MessageID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	deliveryID, err := parseCanonicalUUID("delivery_id", request.DeliveryID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	if err == nil {
		err = session.CompleteMessageFromRun(r.Context(), s.tx, fence, workerTurnWork(request.TurnExecutionRequest), messageID, deliveryID, session.MessageOutcome{Status: request.Status, Code: request.Code, Details: request.Details})
	}
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerSessionControl(w http.ResponseWriter, r *http.Request) {
	var request workerapi.SessionControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session control JSON: %w", err))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state, err := session.ReadControl(r.Context(), s.db, workerExecutionFence(workerFromContext(r.Context()), parsed, request.Lease), request.RunGeneration)
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := workerapi.SessionControlResponse{CorrelationID: request.CorrelationID}
	if state.DispatchHoldID.Valid {
		id, reason := pgvalue.UUIDString(state.DispatchHoldID), state.DispatchHoldReason.String
		response.HoldID = &id
		response.Reason = &reason
	}
	if state.ActiveTurnID.Valid {
		id := pgvalue.UUIDString(state.ActiveTurnID)
		response.TurnID = &id
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) workerWriteSessionOutput(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WriteSessionOutputRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session output JSON: %w", err))
		return
	}
	fence, err := workerLeaseFence(workerFromContext(r.Context()), request.Lease)
	var output session.Output
	if err == nil {
		output, err = session.AppendSessionOutputFromRun(r.Context(), s.tx, fence, request.RunGeneration, request.IdempotencyKey, request.CorrelationID, request.Data)
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	event := projectWorkerSessionEvent(output.Event(), output.DeploymentID())
	writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{CorrelationID: request.CorrelationID, Completed: &event})
}
