package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

// Both controls lock the complete Secret union before the ordered source-ancestor and target Sessions.
// Secret bindings are read again after the Session and Computer fences; a new
// binding invalidates this attempt rather than acquiring a Secret out of order.
func lockWorkerSessionControl(ctx context.Context, work *txWork, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence, targetID pgtype.UUID, interrupt bool) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
	fail := func(err error) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
		return run.LiveSource{}, run.OwnedFinalization{}, nil, err
	}
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return fail(err)
	}
	controls, err := run.LockControlSecrets(ctx, work.tx, workerExecutionFence(worker, parsed, lease), targetID)
	if err != nil {
		return fail(workerControlTargetError(err))
	}
	var source run.LiveSource
	var graph run.OwnedFinalization
	if interrupt {
		_, source, graph, err = controls.LockInterruptionLiveSource(ctx)
	} else {
		_, source, err = controls.LockLiveSource(ctx)
	}
	if err != nil {
		return fail(workerControlTargetError(err))
	}
	// The union operation has already locked all source-ancestor and target
	// Sessions. This validation cannot add a differently ordered Session lock.
	if err = lockWorkerControlActors(ctx, work.q, source, targetID); err != nil {
		return fail(err)
	}
	target := controls.Target()
	lockedTarget, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: source.EnvironmentID(), ID: targetID})
	if err != nil {
		return fail(err)
	}
	if target.ComputerID != lockedTarget.ComputerID || target.CurrentRunID != lockedTarget.CurrentRunID || target.RunGeneration != lockedTarget.RunGeneration {
		return fail(session.ErrAuthority)
	}

	if !interrupt && !target.CurrentRunID.Valid {
		if _, err = computer.LockSessionComputer(ctx, work.tx, computer.SessionComputerRef{EnvironmentID: pgvalue.MustUUIDValue(target.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(target.ComputerID), SessionID: pgvalue.MustUUIDValue(target.ID)}); err != nil {
			return fail(err)
		}
	}
	if err = controls.Recheck(ctx, interrupt); err != nil {
		return fail(err)
	}
	return source, graph, controls.TargetBindings(), nil
}

// workerControlTargetError reports a control target outside the source's
// environment as a missing Session.
func workerControlTargetError(err error) error {
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return &session.OperationError{Code: "session_not_found"}
	}
	return err
}

// Owned Tasks retain their ancestor Actor's lifecycle fence even in a different
// Computer. Locking only the source Computer's Session would let reciprocal
// child controls each hold the other's graph root while waiting on its child.
func lockWorkerControlActors(ctx context.Context, q db.Querier, source run.LiveSource, targetID pgtype.UUID) error {
	actors, err := q.LockWorkerControlActors(ctx, db.LockWorkerControlActorsParams{EnvironmentID: source.EnvironmentID(), SourceRunID: source.RunID(), TargetSessionID: targetID})
	if err != nil {
		return err
	}
	for _, row := range actors {
		if !row.SourceOwnerRunID.Valid {
			continue
		}
		actor := row.Session
		if actor.CurrentRunID != row.SourceOwnerRunID {
			return session.ErrAuthority
		}
		if actor.DispatchHoldID.Valid {
			return &session.OperationError{Code: "session_held"}
		}
		if actor.ActiveTurnID.Valid {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
			if err != nil {
				return err
			}
			if turn.SettlementStartedAt.Valid {
				return &session.OperationError{Code: "turn_unsettled"}
			}
		}
	}
	return nil
}

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
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, graph, _, err := lockWorkerSessionControl(r.Context(), work, workerFromContext(r.Context()), request.Lease, sessionID, true)
		if err != nil {
			return err
		}
		receipt, err = session.InterruptTurn(r.Context(), work.tx, pgvalue.MustUUIDValue(source.EnvironmentID()), pgvalue.MustUUIDValue(sessionID), turnID, request.IdempotencyKey, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
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
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, _, bindings, err := lockWorkerSessionControl(r.Context(), work, workerFromContext(r.Context()), request.Lease, sessionID, false)
		if err != nil {
			return err
		}
		target, err := work.q.GetActor(r.Context(), db.GetActorParams{EnvironmentID: source.EnvironmentID(), ID: sessionID})
		if err != nil {
			return err
		}
		receipt, err = session.ResumeWithLockedSecrets(r.Context(), work.tx, session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID()), SessionID: pgvalue.MustUUIDValue(sessionID)}, IdempotencyKey: request.IdempotencyKey}, HoldID: holdID}, target.ComputerID, bindings)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ResumeSessionResponse{CorrelationID: request.CorrelationID, Completed: &api.SessionResumeReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), HoldID: holdID.String(), Status: receipt.Status}})
}
