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
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Both controls lock the complete Secret union before the ordered source-ancestor and target Sessions.
// Secret bindings are read again after the Session and Computer fences; a new
// binding invalidates this attempt rather than acquiring a Secret out of order.
func lockWorkerSessionControl(ctx context.Context, work *txWork, worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence, targetID pgtype.UUID, interrupt bool) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
	var graph run.OwnedFinalization
	fail := func(err error) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
		return run.LiveSource{}, graph, nil, err
	}
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return fail(err)
	}
	loc, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch})
	if err != nil {
		return fail(staleWorkerRunSource(err))
	}
	target, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(&session.OperationError{Code: "session_not_found"})
	}
	if err != nil {
		return fail(err)
	}
	computerIDs := []pgtype.UUID{loc.ComputerID, target.ComputerID}
	lockedSecrets, err := work.q.LockWorkerControlSecrets(ctx, computerIDs)
	if err != nil {
		return fail(err)
	}
	var authority run.ExecutionAuthority
	if interrupt {
		authority, graph, err = run.LockLiveExecutionForSessionInterruption(ctx, work.tx, workerExecutionFence(worker, parsed, lease), targetID)
	} else {
		authority, err = run.LockLiveExecutionForSession(ctx, work.tx, workerExecutionFence(worker, parsed, lease), targetID)
	}
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return fail(&session.OperationError{Code: "session_not_found"})
	}
	source, err := run.CheckLiveSource(authority, err)
	if err != nil {
		return fail(err)
	}
	// The union operation has already locked all source-ancestor and target
	// Sessions. This validation cannot add a differently ordered Session lock.
	if err = lockWorkerControlActors(ctx, work.q, loc, targetID); err != nil {
		return fail(err)
	}
	lockedTarget, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
	if err != nil {
		return fail(err)
	}
	if target.ComputerID != lockedTarget.ComputerID || target.CurrentRunID != lockedTarget.CurrentRunID || target.RunGeneration != lockedTarget.RunGeneration {
		return fail(session.ErrAuthority)
	}

	if !interrupt && !target.CurrentRunID.Valid {
		if _, err = work.q.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, SessionID: target.ID}); err != nil {
			return fail(err)
		}
	}
	reread, err := work.q.ReadWorkerControlSecrets(ctx, computerIDs)
	if err != nil {
		return fail(err)
	}
	if len(reread) != len(lockedSecrets) {
		return fail(secret.ErrDeliveryUnavailable)
	}
	for i := range reread {
		if reread[i].ComputerID != lockedSecrets[i].ComputerID || reread[i].SecretID != lockedSecrets[i].SecretID || reread[i].PlacementKind != lockedSecrets[i].PlacementKind || reread[i].PlacementTarget != lockedSecrets[i].PlacementTarget {
			return fail(secret.ErrDeliveryUnavailable)
		}
	}
	// Computer FOR UPDATE locks block new binding insertion through the
	// computer_secrets foreign key. After checking the binding identities,
	// ordinary delivery validation can only re-lock the original Secret union.
	validateDelivery := func(runID pgtype.UUID, attempt int32, computerID pgtype.UUID) error {
		_, err := secret.LockAttemptDelivery(ctx, work.q, runID, attempt, computerID)
		return err
	}
	if err = validateDelivery(loc.RunID, loc.AttemptNumber, loc.ComputerID); err != nil {
		return fail(err)
	}
	if interrupt && target.CurrentRunID.Valid && target.CurrentRunID != loc.RunID {
		current, err := work.q.GetRun(ctx, db.GetRunParams{EnvironmentID: loc.EnvironmentID, ID: target.CurrentRunID})
		if err != nil {
			return fail(err)
		}
		if err = validateDelivery(current.ID, current.CurrentAttemptNumber, target.ComputerID); err != nil {
			return fail(err)
		}
	}
	var bindings []db.LockComputerSecretsForAdmissionRow
	for _, row := range lockedSecrets {
		if row.ComputerID == target.ComputerID {
			bindings = append(bindings, db.LockComputerSecretsForAdmissionRow(row))
		}
	}
	return source, graph, bindings, nil
}

// Owned Tasks retain their ancestor Actor's lifecycle fence even in a different
// Computer. Locking only the source Computer's Session would let reciprocal
// child controls each hold the other's graph root while waiting on its child.
func lockWorkerControlActors(ctx context.Context, q db.Querier, loc db.GetLiveRunLeaseLocatorsRow, targetID pgtype.UUID) error {
	actors, err := q.LockWorkerControlActors(ctx, db.LockWorkerControlActorsParams{EnvironmentID: loc.EnvironmentID, SourceRunID: loc.RunID, TargetSessionID: targetID})
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
		receipt, err = session.InterruptTurn(r.Context(), work.q, pgvalue.MustUUIDValue(source.EnvironmentID), pgvalue.MustUUIDValue(sessionID), turnID, request.IdempotencyKey, graph)
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
		target, err := work.q.GetActor(r.Context(), db.GetActorParams{EnvironmentID: source.EnvironmentID, ID: sessionID})
		if err != nil {
			return err
		}
		receipt, err = session.ResumeWithLockedSecrets(r.Context(), work.q, session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(sessionID)}, IdempotencyKey: request.IdempotencyKey}, HoldID: holdID}, target.ComputerID, bindings)
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
