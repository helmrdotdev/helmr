package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Both controls lock the complete Secret union before the ordered source-ancestor and target Sessions.
// Secret bindings are read again after the Session and Workspace fences; a new
// binding invalidates this attempt rather than acquiring a Secret out of order.
func lockWorkerSessionControl(ctx context.Context, work *txWork, worker workerActor, lease workerapi.RunLeaseFence, targetID pgtype.UUID, interrupt bool) (workerRunSourceAuthority, run.OwnedFinalization, []db.LockWorkspaceSecretsForAdmissionRow, error) {
	var graph run.OwnedFinalization
	fail := func(err error) (workerRunSourceAuthority, run.OwnedFinalization, []db.LockWorkspaceSecretsForAdmissionRow, error) {
		return workerRunSourceAuthority{}, graph, nil, err
	}
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return fail(err)
	}
	loc, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
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
	workspaceIDs := []pgtype.UUID{loc.WorkspaceID, target.WorkspaceID}
	lockedSecrets, err := work.q.LockWorkerControlSecrets(ctx, workspaceIDs)
	if err != nil {
		return fail(err)
	}
	sourceInTargetGraph, err := lockWorkerControlActors(ctx, work.q, loc, targetID)
	if err != nil {
		return fail(err)
	}
	lockedTarget, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
	if err != nil {
		return fail(err)
	}
	if target.WorkspaceID != lockedTarget.WorkspaceID || target.CurrentRunID != lockedTarget.CurrentRunID || target.RunGeneration != lockedTarget.RunGeneration {
		return fail(session.ErrAuthority)
	}
	var authority runLeaseClaimAuthority
	// A valid standalone Task owns its Workspace exclusively. A different graph
	// cannot queue a child into that Workspace; child admission rejects an owner.
	// Actor sources and their owned descendants are serialized by the sorted
	// source-ancestor and target Sessions above.
	lockSource := func() error {
		var err error
		authority.run, err = work.q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{ID: loc.RunID, OrgID: loc.OrgID, ProjectID: loc.ProjectID, EnvironmentID: loc.EnvironmentID, WorkspaceID: loc.WorkspaceID})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if err = validateLockedRunLeaseRun(authority.run, pgvalue.UUID(parsed.leaseID), loc, db.RunStatusRunning); err != nil {
			return err
		}
		if err = lockRunLeaseWorkspace(ctx, work.q, &authority, loc); err != nil {
			return err
		}
		return lockRunLeaseAttempt(ctx, work.q, &authority, loc)
	}
	physical := func() error {
		return lockRunLeasePhysicalAuthority(ctx, work.q, worker, pgvalue.UUID(parsed.leaseID), lease.LeaseSequence, loc, &authority)
	}
	if interrupt && target.CurrentRunID.Valid {
		// Sources within the target graph re-lock only after the graph acquires
		// ancestors before descendants, matching child finalization.
		if !sourceInTargetGraph {
			if err = lockSource(); err != nil {
				return fail(staleWorkerRunSource(err))
			}
		}
		tx, ok := work.tx.(pgx.Tx)
		if !ok {
			return fail(errors.New("session control transaction does not expose PostgreSQL authority"))
		}
		graph, err = run.LockOwnedFinalizationWithRuntimeFence(ctx, tx, run.OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RunID: pgvalue.MustUUIDValue(target.CurrentRunID)}, func() error {
			if sourceInTargetGraph {
				if err := lockSource(); err != nil {
					return err
				}
			}
			return physical()
		})
	} else {
		err = lockSource()
		if err == nil {
			err = physical()
		}
	}
	source, err := validateWorkerRunSource(authority, loc, err)
	if err != nil {
		return fail(err)
	}
	if !interrupt && !target.CurrentRunID.Valid {
		if _, err = work.q.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{EnvironmentID: target.EnvironmentID, WorkspaceID: target.WorkspaceID, SessionID: target.ID}); err != nil {
			return fail(err)
		}
	}
	reread, err := work.q.ReadWorkerControlSecrets(ctx, workspaceIDs)
	if err != nil {
		return fail(err)
	}
	if len(reread) != len(lockedSecrets) {
		return fail(secret.ErrDeliveryUnavailable)
	}
	for i := range reread {
		if reread[i].WorkspaceID != lockedSecrets[i].WorkspaceID || reread[i].SecretID != lockedSecrets[i].SecretID || reread[i].PlacementKind != lockedSecrets[i].PlacementKind || reread[i].PlacementTarget != lockedSecrets[i].PlacementTarget {
			return fail(secret.ErrDeliveryUnavailable)
		}
	}
	// Workspace FOR UPDATE locks block new binding insertion through the
	// workspace_secrets foreign key. After checking the binding identities,
	// ordinary delivery validation can only re-lock the original Secret union.
	validateDelivery := func(runID pgtype.UUID, attempt int32, workspaceID pgtype.UUID) error {
		_, err := secret.LockAttemptDelivery(ctx, work.q, runID, attempt, workspaceID)
		return err
	}
	if err = validateDelivery(loc.RunID, loc.AttemptNumber, loc.WorkspaceID); err != nil {
		return fail(err)
	}
	if interrupt && target.CurrentRunID.Valid && target.CurrentRunID != loc.RunID {
		current, err := work.q.GetRun(ctx, db.GetRunParams{EnvironmentID: loc.EnvironmentID, ID: target.CurrentRunID})
		if err != nil {
			return fail(err)
		}
		if err = validateDelivery(current.ID, current.CurrentAttemptNumber, target.WorkspaceID); err != nil {
			return fail(err)
		}
	}
	var bindings []db.LockWorkspaceSecretsForAdmissionRow
	for _, row := range lockedSecrets {
		if row.WorkspaceID == target.WorkspaceID {
			bindings = append(bindings, db.LockWorkspaceSecretsForAdmissionRow(row))
		}
	}
	return source, graph, bindings, nil
}

// Owned Tasks retain their ancestor Actor's lifecycle fence even in a different
// Workspace. Locking only the source Workspace's Session would let reciprocal
// child controls each hold the other's graph root while waiting on its child.
func lockWorkerControlActors(ctx context.Context, q db.Querier, loc db.GetLiveRunLeaseLocatorsRow, targetID pgtype.UUID) (bool, error) {
	actors, err := q.LockWorkerControlActors(ctx, db.LockWorkerControlActorsParams{EnvironmentID: loc.EnvironmentID, SourceRunID: loc.RunID, TargetSessionID: targetID})
	if err != nil {
		return false, err
	}
	sourceInTargetGraph := false
	for _, row := range actors {
		if !row.SourceOwnerRunID.Valid {
			continue
		}
		sourceInTargetGraph = sourceInTargetGraph || row.Session.ID == targetID
		actor := row.Session
		if actor.CurrentRunID != row.SourceOwnerRunID {
			return false, session.ErrAuthority
		}
		if actor.DispatchHoldID.Valid {
			return false, &session.OperationError{Code: "session_held"}
		}
		if actor.ActiveTurnID.Valid {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
			if err != nil {
				return false, err
			}
			if turn.SettlementStartedAt.Valid {
				return false, &session.OperationError{Code: "turn_unsettled"}
			}
		}
	}
	return sourceInTargetGraph, nil
}

func (s *Server) workerInterruptSessionTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.InterruptSessionTurnRequest
	if err := decodeWorkerActorRequest(r, &request, "Turn interrupt"); err != nil {
		writeError(w, badRequest(err))
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
	if err := decodeWorkerActorRequest(r, &request, "Session resume"); err != nil {
		writeError(w, badRequest(err))
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
		receipt, err = session.ResumeWithLockedSecrets(r.Context(), work.q, session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(sessionID)}, IdempotencyKey: request.IdempotencyKey}, HoldID: holdID}, target.WorkspaceID, bindings)
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
