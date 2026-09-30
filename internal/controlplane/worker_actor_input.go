package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Secret locks precede physical authority; source and target Sessions are then
// locked together, in UUID order, before the source Run lineage.
func authorizeWorkerSessionOperation(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence, targetID, targetComputerID pgtype.UUID) (run.LiveSource, error) {
	q := db.New(tx)
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return run.LiveSource{}, err
	}
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch})
	if err != nil {
		return run.LiveSource{}, staleWorkerRunSource(err)
	}
	computerIDs := []pgtype.UUID{loc.ComputerID}
	if targetComputerID.Valid {
		computerIDs = append(computerIDs, targetComputerID)
	}
	_, err = q.LockWorkerControlSecrets(ctx, computerIDs)
	if err != nil {
		return run.LiveSource{}, err
	}
	var authority run.Execution
	if targetID.Valid {
		authority, err = run.LockLiveExecutionForSession(ctx, tx, workerExecutionFence(worker, parsed, lease), targetID)
	} else if targetComputerID.Valid {
		authority, err = run.LockLiveExecutionForComputer(ctx, tx, workerExecutionFence(worker, parsed, lease), targetComputerID)
	} else {
		authority, err = run.LockLiveExecution(ctx, tx, workerExecutionFence(worker, parsed, lease))
	}
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		if targetID.Valid {
			return run.LiveSource{}, &session.OperationError{Code: "session_not_found"}
		}
		return run.LiveSource{}, errActorStartComputerNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return run.LiveSource{}, run.ErrStaleSource
	}
	if err != nil {
		return run.LiveSource{}, err
	}
	source, err := authority.LiveSource()
	if err != nil {
		return run.LiveSource{}, err
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID); err != nil {
		return run.LiveSource{}, err
	}
	actor := authority.Session()
	if actor.ID.Valid {
		if actor.DispatchHoldID.Valid {
			return run.LiveSource{}, &session.OperationError{Code: "session_held"}
		}
		if actor.ActiveTurnID.Valid {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
			if err != nil {
				return run.LiveSource{}, err
			}
			if turn.SettlementStartedAt.Valid {
				return run.LiveSource{}, &session.OperationError{Code: "turn_unsettled"}
			}
		}
	}
	return source, nil
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerSessionOperation(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease, pgvalue.UUID(targetID), pgtype.UUID{})
		if err != nil {
			return err
		}
		command.Target = session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID()), SessionID: targetID}
		command.SourceRunID = pgvalue.MustUUIDValue(source.RunID())
		receipt, err = session.Admit(r.Context(), work.tx, command)
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
