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
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Source-only Secret locks precede physical authority; source and target
// Sessions are then locked together, in UUID order, before the source Run
// lineage.
func authorizeWorkerSessionOperation(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence, targetID pgtype.UUID) (run.LiveSource, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return run.LiveSource{}, err
	}
	secrets, err := run.LockSourceSecretsForSession(ctx, tx, workerExecutionFence(worker, parsed, lease), targetID)
	if err != nil {
		return run.LiveSource{}, err
	}
	authority, source, err := secrets.LockLiveSource(ctx)
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return run.LiveSource{}, &session.OperationError{Code: "session_not_found"}
	}
	if err != nil {
		return run.LiveSource{}, err
	}
	if err = secrets.ValidateSourceDelivery(ctx); err != nil {
		return run.LiveSource{}, err
	}
	if err = session.CheckSourceSession(ctx, db.New(tx), authority.Session()); err != nil {
		return run.LiveSource{}, err
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
		source, err := authorizeWorkerSessionOperation(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease, pgvalue.UUID(targetID))
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
