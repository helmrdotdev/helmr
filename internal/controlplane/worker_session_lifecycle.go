package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Secret locks precede Computer/Instance and logical scope locks. Stop remains
// observable so admitted callback acknowledgements can settle their durable work.
func lockWorkerSessionExecution(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence) (run.Execution, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return run.Execution{}, err
	}
	locator, err := run.LocateLiveExecution(ctx, tx, workerExecutionFence(worker, parsed, lease))
	if err != nil {
		return run.Execution{}, staleActorOutputAppend(err)
	}
	if !locator.SessionID().Valid {
		return run.Execution{}, run.ErrTurnScope
	}
	secrets, err := locator.LockSecrets(ctx)
	if err != nil {
		return run.Execution{}, err
	}
	a, err := secrets.LockExecution(ctx)
	if err != nil {
		return run.Execution{}, staleRunLeaseClaim(err)
	}
	if !a.Session().ID.Valid || a.Run().EntrypointKind != "actor" || !a.Attempt().EntrypointEnteredAt.Valid || a.Lease().FinalizationOperationID.Valid {
		return run.Execution{}, run.ErrTurnScope
	}
	return a, nil
}

func workerTurnScope(authority run.Execution, request workerapi.TurnExecutionRequest) (run.TurnScope, error) {
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return run.TurnScope{}, err
	}
	if request.RunGeneration <= 0 {
		return run.TurnScope{}, run.ErrTurnScope
	}
	return run.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.Session().EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.Session().ID), TurnID: turnID, RunID: pgvalue.MustUUIDValue(authority.Run().ID), AttemptNumber: authority.Attempt().Number, RunGeneration: request.RunGeneration}, nil
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
	if errors.Is(err, run.ErrStaleSource) {
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
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn readiness JSON: %w", err))
		return
	}
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request)
		if err != nil {
			return err
		}
		if a.Run().Status != db.RunStatusRunning || a.Lease().Status != db.RunLeaseStatusRunning {
			return run.ErrTurnScope
		}
		_, err = session.DeclareMessageReady(r.Context(), work.tx, scope, pgvalue.MustUUIDValue(a.Lease().ID))
		return err
	})
	s.writeWorkerSessionCommand(w, request.CorrelationID, err)
}
func (s *Server) workerBeginTurnSettlement(w http.ResponseWriter, r *http.Request) {
	var request workerapi.TurnExecutionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Turn settlement JSON: %w", err))
		return
	}
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request)
		if err != nil {
			return err
		}
		_, err = session.BeginSettlement(r.Context(), work.tx, scope)
		return err
	})
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request.TurnExecutionRequest)
		if err != nil {
			return err
		}
		message, err = session.ClaimMessage(r.Context(), work.tx, scope, pgvalue.MustUUIDValue(a.Lease().ID), deliveryID)
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		scope, err := workerTurnScope(a, request.TurnExecutionRequest)
		if err != nil {
			return err
		}
		_, err = session.CompleteMessage(r.Context(), work.tx, scope, pgvalue.MustUUIDValue(a.Lease().ID), messageID, deliveryID, session.MessageOutcome{Status: request.Status, Code: request.Code, Details: request.Details})
		return err
	})
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
	worker := workerFromContext(r.Context())
	// This is an advisory observation. Finalization separately locks and proves
	// the exact hold; polling must not contend with shared worker dispatch locks.
	state, err := s.db.ReadWorkerSessionControl(r.Context(), db.ReadWorkerSessionControlParams{
		RunLeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, RunGeneration: request.RunGeneration,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = run.ErrTurnScope
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
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session output JSON: %w", err))
		return
	}
	var event api.SessionEvent
	var rejection string
	err := s.inTx(r.Context(), func(work *txWork) error {
		a, err := lockWorkerSessionExecution(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease)
		if err != nil {
			return err
		}
		if a.Run().Status != db.RunStatusRunning || a.Lease().Status != db.RunLeaseStatusRunning {
			return run.ErrTurnScope
		}
		key := request.IdempotencyKey
		if key == "" {
			key = request.CorrelationID
		}
		receipt, err := session.AppendSessionOutput(r.Context(), work.tx, run.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(a.Session().EnvironmentID), SessionID: pgvalue.MustUUIDValue(a.Session().ID), RunID: pgvalue.MustUUIDValue(a.Run().ID), AttemptNumber: a.Attempt().Number, RunGeneration: request.RunGeneration}, key, request.Data)
		if err != nil {
			return err
		}
		rejection = receipt.Code
		if rejection == "" {
			event = projectWorkerSessionEvent(receipt.Event, a.Run().DeploymentID)
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
