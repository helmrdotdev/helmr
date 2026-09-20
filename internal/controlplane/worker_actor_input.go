package controlplane

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
)

// Lock both source and addressed Session in UUID order before physical Run
// authority. Reciprocal Actor sends must not invert Session -> Run lock order.
func authorizeWorkerSessionOperation(ctx context.Context, q db.Querier, worker workerActor, lease workerapi.RunLeaseFence, targetID pgtype.UUID) (workerRunSourceAuthority, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return workerRunSourceAuthority{}, err
	}
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
	if err != nil {
		return workerRunSourceAuthority{}, staleWorkerRunSource(err)
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.WorkspaceID); err != nil {
		return workerRunSourceAuthority{}, err
	}
	actors, err := q.LockWorkerSessionOperationActors(ctx, db.LockWorkerSessionOperationActorsParams{EnvironmentID: loc.EnvironmentID, TargetSessionID: targetID, SourceWorkspaceID: loc.WorkspaceID})
	if err != nil {
		return workerRunSourceAuthority{}, err
	}
	for _, actor := range actors {
		if actor.WorkspaceID != loc.WorkspaceID {
			continue
		}
		if actor.DispatchHoldID.Valid {
			return workerRunSourceAuthority{}, &session.OperationError{Code: "session_held"}
		}
		if actor.ActiveTurnID.Valid {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
			if err != nil {
				return workerRunSourceAuthority{}, err
			}
			if turn.SettlementStartedAt.Valid {
				return workerRunSourceAuthority{}, &session.OperationError{Code: "turn_unsettled"}
			}
		}
	}
	return authorizeWorkerRunSource(ctx, q, worker, lease)
}
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
	if err := decodeWorkerActorRequest(r, &request, "Session submission"); err != nil {
		writeError(w, badRequest(err))
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerSessionOperation(r.Context(), work.q, workerFromContext(r.Context()), request.Lease, pgvalue.UUID(targetID))
		if err != nil {
			return err
		}
		command.Target = session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: targetID}
		command.SourceRunID = pgvalue.MustUUIDValue(source.RunID)
		receipt, err = session.Admit(r.Context(), work.q, command)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
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
