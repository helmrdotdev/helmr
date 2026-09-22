package session

import (
	"context"
	"encoding/json"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5/pgtype"
)

func Close(ctx context.Context, q db.Querier, request ControlRequest) (ControlReceipt, error) {
	actor, err := lockSession(ctx, q, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	claim, err := claimOperation(ctx, q, request, "session.close", struct{}{})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, Status: "accepted"}
	if actor.Status != "open" && actor.Status != "closing" && actor.Status != "closed" {
		receipt.Code = "session_not_open"
	} else if actor.Status == "open" {
		actor, err = q.BeginActorClose(ctx, db.BeginActorCloseParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"close_sequence": actor.CloseSequence.Int64})
		if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, "session.closing", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
	}
	if actor.Status == "closing" {
		err = q.CreateActorCloseReconcileOutbox(ctx, db.CreateActorCloseReconcileOutboxParams{ID: claim.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
		if err != nil {
			return receipt, err
		}
	}
	return receipt, finishOperation(ctx, q, claim, receipt)
}

func Resume(ctx context.Context, q db.Querier, request ResumeRequest) (ControlReceipt, error) {
	locator, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: pgvalue.UUID(request.SessionID)})
	if err != nil {
		return ControlReceipt{}, err
	}
	// Continuation consumes secrets: take their complete admission set before Session.
	bindings, err := q.LockWorkspaceSecretsForAdmission(ctx, locator.WorkspaceID)
	if err != nil {
		return ControlReceipt{}, err
	}
	return ResumeWithLockedSecrets(ctx, q, request, locator.WorkspaceID, bindings)
}

// ResumeWithLockedSecrets consumes the target's complete admission bindings,
// locked before Session authority by a cross-Session caller.
func ResumeWithLockedSecrets(ctx context.Context, q db.Querier, request ResumeRequest, workspaceID pgtype.UUID, bindings []db.LockWorkspaceSecretsForAdmissionRow) (ControlReceipt, error) {
	actor, err := lockSession(ctx, q, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	if actor.WorkspaceID != workspaceID {
		return ControlReceipt{}, ErrAuthority
	}
	claim, err := claimOperation(ctx, q, request.ControlRequest, "session.resume", struct {
		HoldID uuid.UUID `json:"hold_id"`
	}{request.HoldID})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, HoldID: &request.HoldID, Status: "accepted"}
	switch {
	case actor.CancelRequestedAt.Valid:
		receipt.Code = "session_not_open"
	case actor.DispatchHoldID != pgvalue.UUID(request.HoldID):
		receipt.Code = "stale_hold"
	case actor.Status != "open" && actor.Status != "closing":
		receipt.Code = "session_not_open"
	case actor.ActiveTurnID.Valid || actor.CurrentRunID.Valid || actor.DispatchHoldReason.String == "interrupt_requested":
		receipt.Code = "not_settled"
	case actor.DispatchHoldReason.String == "recovery_required":
		receipt.Code = "recovery_required"
	default:
		workspace, err := q.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{EnvironmentID: actor.EnvironmentID, WorkspaceID: actor.WorkspaceID, SessionID: actor.ID})
		if err != nil {
			return receipt, err
		}
		excluded, err := q.SessionWriterExcluded(ctx, actor.WorkspaceID)
		if err != nil {
			return receipt, err
		}
		if (!excluded.Valid || !excluded.Bool) || workspace.DirtyState != db.WorkspaceDirtyStateClean || workspace.Status != db.WorkspaceStatusActive || workspace.DesiredState != db.WorkspaceDesiredStateActive || !workspace.HeadVersionID.Valid || !bindingsCanAdmit(actor, bindings) {
			receipt.Code = "not_settled"
			break
		}
		actor, err = q.ClearSessionDispatchHold(ctx, db.ClearSessionDispatchHoldParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID, DispatchHoldID: actor.DispatchHoldID})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"hold_id": request.HoldID})
		if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, "session.resumed", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
		if actor.Status == "closing" {
			err = q.CreateActorCloseReconcileOutbox(ctx, db.CreateActorCloseReconcileOutboxParams{ID: claim.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
		} else {
			_, err = CreateContinuation(ctx, q, actor, db.LockActorInputWorkspaceRow{ID: workspace.ID}, bindings)
		}
		if err != nil {
			return receipt, err
		}
	}
	return receipt, finishOperation(ctx, q, claim, receipt)
}

// Recover accepts reconciliation only after the database's existing physical
// cleanup observations exclude the old writer. The requested committed head is
// the explicit recovery point; unpublished files and old memory are not restored.
// A customer boolean is no proof of physical cleanup.
func Recover(ctx context.Context, q db.Querier, request RecoverRequest, graph run.OwnedFinalization) (ControlReceipt, error) {
	if request.WorkspaceVersionID == uuid.Nil() || request.HoldID == uuid.Nil() || strings.TrimSpace(request.ReconciliationRef) == "" || len(request.ReconciliationRef) > 4096 || (request.TurnID == nil && request.Disposition != "") || (request.TurnID != nil && request.Disposition != "failed" && request.Disposition != "interrupted") {
		return ControlReceipt{}, &OperationError{Code: "invalid_request"}
	}
	actor, err := lockSession(ctx, q, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	claim, err := claimOperation(ctx, q, request.ControlRequest, "session.recover", struct {
		HoldID             uuid.UUID  `json:"hold_id"`
		TurnID             *uuid.UUID `json:"turn_id"`
		WorkspaceVersionID uuid.UUID  `json:"workspace_version_id"`
		ReconciliationRef  string     `json:"reconciliation_ref"`
		Disposition        string     `json:"disposition,omitempty"`
	}{request.HoldID, request.TurnID, request.WorkspaceVersionID, request.ReconciliationRef, request.Disposition})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, TurnID: request.TurnID, Status: "accepted"}
	var turnID pgtype.UUID
	if request.TurnID != nil {
		turnID = pgvalue.UUID(*request.TurnID)
	}
	switch {
	case actor.DispatchHoldID != pgvalue.UUID(request.HoldID):
		receipt.Code = "stale_hold"
	case actor.ActiveTurnID != turnID:
		receipt.Code = "turn_not_active"
	case actor.Status != "open" && actor.Status != "closing":
		receipt.Code = "session_not_open"
	case actor.DispatchHoldReason.String == "recovered" || actor.DispatchHoldReason.String == "interrupted":
		receipt.Code = "not_settled"
	}
	if receipt.Code != "" {
		return receipt, finishOperation(ctx, q, claim, receipt)
	}
	workspace, err := q.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{EnvironmentID: actor.EnvironmentID, WorkspaceID: actor.WorkspaceID, SessionID: actor.ID})
	if err != nil {
		return receipt, err
	}
	committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: actor.EnvironmentID, WorkspaceID: actor.WorkspaceID, ID: pgvalue.UUID(request.WorkspaceVersionID)})
	if err != nil {
		return receipt, err
	}
	recoverLostComputer := workspace.Status == db.WorkspaceStatusRecoveryRequired &&
		workspace.DirtyState == db.WorkspaceDirtyStateDirtyStateLost
	cleanComputer := workspace.Status == db.WorkspaceStatusActive &&
		workspace.DirtyState == db.WorkspaceDirtyStateClean
	if !committed || workspace.HeadVersionID != pgvalue.UUID(request.WorkspaceVersionID) || (!recoverLostComputer && !cleanComputer) {
		receipt.Code = "not_settled"
		return receipt, finishOperation(ctx, q, claim, receipt)
	}
	var turn db.SessionTurn
	if turnID.Valid {
		turn, err = q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: turnID})
		if err != nil {
			return receipt, err
		}
		want := "failed"
		if turn.InterruptRequestedAt.Valid {
			want = "interrupted"
		}
		if turn.Status != "running" || turn.Sequence != actor.CommittedInputSequence+1 || request.Disposition != want {
			receipt.Code = "turn_unsettled"
			return receipt, finishOperation(ctx, q, claim, receipt)
		}
	}
	if actor.CurrentRunID.Valid {
		if _, err = graph.RetireHeldActorIfUnleased(ctx, request.HoldID); err != nil {
			return receipt, err
		}
		current, err := q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID})
		if err != nil {
			return receipt, err
		}
		switch current.Status {
		case db.RunStatusSucceeded, db.RunStatusFailed, db.RunStatusSystemFailed, db.RunStatusExpired, db.RunStatusCancelled:
		default:
			receipt.Code = "not_settled"
			return receipt, finishOperation(ctx, q, claim, receipt)
		}
	}
	excluded, err := q.SessionWriterExcluded(ctx, actor.WorkspaceID)
	if err != nil {
		return receipt, err
	}
	if !excluded.Valid || !excluded.Bool {
		receipt.Code = "not_settled"
		return receipt, finishOperation(ctx, q, claim, receipt)
	}
	if recoverLostComputer {
		affected, err := q.ReconcileSessionComputer(ctx, db.ReconcileSessionComputerParams{
			EnvironmentID: actor.EnvironmentID, WorkspaceID: actor.WorkspaceID,
			SessionID: actor.ID, HeadVersionID: workspace.HeadVersionID,
		})
		if err != nil {
			return receipt, err
		}
		if affected != 1 {
			return receipt, ErrAuthority
		}
	}
	var sequence pgtype.Int8
	if turnID.Valid {
		want := request.Disposition
		if err = finishRecoveredMessages(ctx, q, actor, turn.ID); err != nil {
			return receipt, err
		}
		body := map[string]any{"workspace_version_id": request.WorkspaceVersionID, "hold_id": request.HoldID, "reconciliation_ref": request.ReconciliationRef}
		if want == "failed" {
			body["error"] = map[string]any{"code": "execution_lost"}
		}
		raw, _ := json.Marshal(body)
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn."+want, raw, workspace.HeadVersionID)
		if err != nil {
			return receipt, err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, Status: want, EventID: event.ID, Fingerprint: pgvalue.Text(sha256sum.FormatDigest(claim.RequestFingerprint))}); err != nil {
			return receipt, err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	newHold := uuid.NewV7()
	actor, err = q.CompleteSessionRecovery(ctx, db.CompleteSessionRecoveryParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, HoldID: actor.DispatchHoldID, TurnID: turnID, InputSequence: sequence, NewHoldID: pgvalue.UUID(newHold)})
	if err != nil {
		return receipt, err
	}
	body, _ := json.Marshal(map[string]any{"hold_id": newHold, "previous_hold_id": request.HoldID, "workspace_version_id": request.WorkspaceVersionID, "reconciliation_ref": request.ReconciliationRef, "computer_reconciled": recoverLostComputer})
	if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, "session.recovered", body, workspace.HeadVersionID); err != nil {
		return receipt, err
	}
	if actor.Status == "closing" {
		if err = q.CreateActorCloseReconcileOutbox(ctx, db.CreateActorCloseReconcileOutboxParams{ID: claim.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID}); err != nil {
			return receipt, err
		}
	}
	receipt.HoldID = &newHold
	return receipt, finishOperation(ctx, q, claim, receipt)
}

// CompleteInterruption publishes the held Turn only after the caller verifies the
// exact live finalization receipt and captures the quiesced execution's Workspace.
// The caller holds the Session and owned Run graph locks in the same transaction.
func CompleteInterruption(ctx context.Context, q db.Querier, actor db.Session, versionID pgtype.UUID, fingerprint string) error {
	var sequence pgtype.Int8
	if actor.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
		if err != nil {
			return err
		}
		if turn.Status != "running" || turn.Sequence != actor.CommittedInputSequence+1 || !turn.InterruptRequestedAt.Valid {
			return &OperationError{Code: "stale_execution"}
		}
		body, _ := json.Marshal(map[string]any{"workspace_version_id": pgvalue.UUIDString(versionID), "hold_id": pgvalue.UUIDString(actor.DispatchHoldID)})
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.interrupted", body, versionID)
		if err != nil {
			return err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, Status: "interrupted", EventID: event.ID, Fingerprint: pgvalue.Text(fingerprint)}); err != nil {
			return err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	previousHold := actor.DispatchHoldID
	held, err := q.CompleteSessionInterruption(ctx, db.CompleteSessionInterruptionParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID, RunGeneration: actor.RunGeneration, HoldID: actor.DispatchHoldID, TurnID: actor.ActiveTurnID, InputSequence: sequence, NewHoldID: pgvalue.UUID(uuid.NewV7())})
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"hold_id": pgvalue.UUIDString(held.DispatchHoldID), "previous_hold_id": pgvalue.UUIDString(previousHold), "reason": "interrupted", "workspace_version_id": pgvalue.UUIDString(versionID)})
	_, err = appendLifecycleEvent(ctx, q, held, pgtype.UUID{}, pgtype.UUID{}, "session.held", body, versionID)
	return err
}
