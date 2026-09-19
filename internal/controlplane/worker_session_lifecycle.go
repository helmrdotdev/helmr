package controlplane

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
)

// These commands use the same Secret -> Session owner -> Run -> Workspace ->
// physical authority order as output/commit. Stop is deliberately not an error
// here: admitted callback acknowledgments and control observation must still work.
func lockWorkerSessionExecution(ctx context.Context, q db.Querier, worker workerActor, lease workerapi.RunLeaseFence) (runLeaseClaimAuthority, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
	if err != nil || !loc.SessionID.Valid {
		return runLeaseClaimAuthority{}, staleActorOutputAppend(err)
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.WorkspaceID); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	owner, err := lockRunFinalizationOwner(ctx, q, loc)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	authority, err := lockRenewableRunLeaseAuthority(ctx, q, worker, pgvalue.UUID(parsed.leaseID), lease.LeaseSequence, loc)
	if err != nil {
		return authority, err
	}
	authority.actor = owner.actor
	if !authority.actor.ID.Valid || authority.run.EntrypointKind != "actor" || authority.actor.CurrentRunID != authority.run.ID || authority.attempt.TerminalAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid || authority.runLease.FinalizationOperationID.Valid {
		return authority, session.ErrTurnScope
	}
	return authority, nil
}

func workerTurnScope(authority runLeaseClaimAuthority, request workerapi.TurnExecutionRequest) (session.TurnScope, error) {
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return session.TurnScope{}, err
	}
	if request.RunGeneration <= 0 {
		return session.TurnScope{}, session.ErrTurnScope
	}
	return session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.actor.EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.actor.ID), TurnID: turnID, RunID: pgvalue.MustUUIDValue(authority.run.ID), AttemptNumber: authority.attempt.Number, RunGeneration: request.RunGeneration}, nil
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
func (s *Server) writeWorkerSessionCommand(w http.ResponseWriter, correlation string, err error) {
	if errors.Is(err, errStaleWorkerRunSource) {
		err = &session.OperationError{Code: "stale_execution"}
	}
	response := workerapi.TurnCommandResponse{CorrelationID: correlation, Accepted: err == nil}
	if err != nil {
		if failure, ok := actorOutputAppendFailure(err); ok {
			response.Failed = &failure
		} else if writeStaleWorkerClaims(w, err) {
			return
		} else if errors.Is(err, errStaleActorOutputAppend) || errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(err))
			return
		} else {
			s.log.Error("Session worker command", "error", err)
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) workerTurnMessagesReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnExecutionRequest
	if err := decodeWorkerActorRequest(r, &request, "Turn readiness"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request)
		if err != nil {
			return err
		}
		if a.run.Status != db.RunStatusRunning || a.runLease.Status != db.RunLeaseStatusRunning {
			return session.ErrTurnScope
		}
		_, err = session.DeclareMessageReady(r.Context(), work.q, scope, pgvalue.MustUUIDValue(a.runLease.ID))
		return err
	})
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerBeginTurnSettlement(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnExecutionRequest
	if err := decodeWorkerActorRequest(r, &request, "Turn settlement"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request)
		if err != nil {
			return err
		}
		_, err = session.BeginSettlement(r.Context(), work.q, scope)
		return err
	})
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerClaimTurnMessage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ClaimTurnMessageRequest
	if err := decodeWorkerActorRequest(r, &request, "message claim"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	deliveryID, err := parseCanonicalUUID("delivery_id", request.DeliveryID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var message db.SessionMessage
	err = s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request.TurnExecutionRequest)
		if err != nil {
			return err
		}
		message, err = session.ClaimMessage(r.Context(), work.q, scope, pgvalue.MustUUIDValue(a.runLease.ID), deliveryID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
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
	if err := decodeWorkerActorRequest(r, &request, "message completion"); err != nil {
		writeError(w, badRequest(err))
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request.TurnExecutionRequest)
		if err != nil {
			return err
		}
		_, err = session.CompleteMessage(r.Context(), work.q, scope, pgvalue.MustUUIDValue(a.runLease.ID), messageID, deliveryID, session.MessageOutcome{Status: request.Status, Code: request.Code, Details: request.Details})
		return err
	})
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerSessionControl(w http.ResponseWriter, r *http.Request) {
	var request workerapi.SessionControlRequest
	if err := decodeWorkerActorRequest(r, &request, "Session control"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	// This is an advisory observation. Finalization separately locks and proves
	// the exact hold; polling must not contend with shared worker placement locks.
	state, err := s.db.ReadWorkerSessionControl(r.Context(), db.ReadWorkerSessionControlParams{
		RunLeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch, RunGeneration: request.RunGeneration,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = session.ErrTurnScope
		}
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
	if err := decodeWorkerActorRequest(r, &request, "Session output"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var event api.SessionEvent
	var rejection string
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.q, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		if a.run.Status != db.RunStatusRunning || a.runLease.Status != db.RunLeaseStatusRunning {
			return session.ErrTurnScope
		}
		key := request.IdempotencyKey
		if key == "" {
			key = request.CorrelationID
		}
		receipt, err := session.AppendSessionOutput(r.Context(), work.q, session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(a.actor.EnvironmentID), SessionID: pgvalue.MustUUIDValue(a.actor.ID), RunID: pgvalue.MustUUIDValue(a.run.ID), AttemptNumber: a.attempt.Number, RunGeneration: request.RunGeneration}, key, request.Data)
		if err != nil {
			return err
		}
		rejection = receipt.Code
		if rejection == "" {
			event = projectWorkerSessionEvent(receipt.Event, a.run.DeploymentID)
		}
		return nil
	})
	if err == nil && rejection != "" {
		err = &session.OperationError{Code: rejection}
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{CorrelationID: request.CorrelationID, Completed: &event})
}
