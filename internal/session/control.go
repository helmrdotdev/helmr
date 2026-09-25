package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
		err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: claim.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
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
			err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: claim.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
		} else if CanStartContinuation(actor) {
			_, err = CreateContinuation(ctx, q, actor, db.LockActorInputWorkspaceRow{ID: workspace.ID}, bindings)
		}
		if err != nil {
			return receipt, err
		}
	}
	return receipt, finishOperation(ctx, q, claim, receipt)
}

// CompleteInterruption publishes the held Turn only after the caller verifies the
// exact captured finalization, or a cancelled unleased execution with physical
// writer exclusion and a committed retained head, in the caller's transaction.
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
		if err := finishUnsettledMessages(ctx, q, actor, turn.ID, "execution_interrupted"); err != nil {
			return err
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
