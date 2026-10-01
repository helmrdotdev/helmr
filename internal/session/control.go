package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func Close(ctx context.Context, tx pgx.Tx, request ControlRequest) (ControlReceipt, error) {
	q := db.New(tx)
	session, err := lockSession(ctx, tx, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	claim, err := claimOperation(ctx, tx, request, "session.close", struct{}{})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, Status: "accepted"}
	if session.Status != "open" && session.Status != "closing" && session.Status != "closed" {
		receipt.Code = "session_not_open"
	} else if session.Status == "open" {
		session, err = q.BeginSessionClose(ctx, db.BeginSessionCloseParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"close_sequence": session.CloseSequence.Int64})
		if _, err = appendLifecycleEvent(ctx, q, session, pgtype.UUID{}, pgtype.UUID{}, "session.closing", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
	}
	if session.Status == "closing" {
		err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: claim.ID, EnvironmentID: session.EnvironmentID, SessionID: session.ID})
		if err != nil {
			return receipt, err
		}
	}
	return receipt, finishOperation(ctx, tx, claim, receipt)
}

func resume(ctx context.Context, tx pgx.Tx, request ResumeRequest) (ControlReceipt, error) {
	q := db.New(tx)
	locator, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), ID: pgvalue.UUID(request.SessionID)})
	if err != nil {
		return ControlReceipt{}, err
	}
	// Continuation consumes secrets: take their complete admission set before Session.
	bindings, err := q.LockComputerSecretsForAdmission(ctx, locator.ComputerID)
	if err != nil {
		return ControlReceipt{}, err
	}
	return resumeWithLockedSecrets(ctx, tx, request, locator.ComputerID, bindings)
}

// resumeWithLockedSecrets consumes the target's complete admission bindings,
// locked before Session authority by a cross-Session caller.
func resumeWithLockedSecrets(ctx context.Context, tx pgx.Tx, request ResumeRequest, computerID pgtype.UUID, bindings []db.LockComputerSecretsForAdmissionRow) (ControlReceipt, error) {
	q := db.New(tx)
	session, err := lockSession(ctx, tx, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	if session.ComputerID != computerID {
		return ControlReceipt{}, ErrAuthority
	}
	claim, err := claimOperation(ctx, tx, request.ControlRequest, "session.resume", struct {
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
	case session.CancelRequestedAt.Valid:
		receipt.Code = "session_not_open"
	case session.DispatchHoldID != pgvalue.UUID(request.HoldID):
		receipt.Code = "stale_hold"
	case session.Status != "open" && session.Status != "closing":
		receipt.Code = "session_not_open"
	case session.ActiveTurnID.Valid || session.CurrentRunID.Valid || session.DispatchHoldReason.String == "interrupt_requested":
		receipt.Code = "not_settled"
	case session.DispatchHoldReason.String == "recovery_required":
		receipt.Code = "recovery_required"
	default:
		locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
		if err != nil {
			return receipt, err
		}
		sessionComputer := locked.Computer()
		excluded, err := q.SessionExecutionScopesReconciled(ctx, session.ID)
		if err != nil {
			return receipt, err
		}
		if (!excluded) || (sessionComputer.DirtyState == db.ComputerDirtyStateDirtyStateLost) || sessionComputer.Status != db.ComputerStatusActive || sessionComputer.DesiredState != db.ComputerDesiredStateActive || !sessionComputer.HeadDiskVersionID.Valid || !bindingsCanAdmit(session, bindings) {
			receipt.Code = "not_settled"
			break
		}
		session, err = q.ClearSessionDispatchHold(ctx, db.ClearSessionDispatchHoldParams{EnvironmentID: session.EnvironmentID, ID: session.ID, DispatchHoldID: session.DispatchHoldID})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"hold_id": request.HoldID})
		if _, err = appendLifecycleEvent(ctx, q, session, pgtype.UUID{}, pgtype.UUID{}, "session.resumed", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
		if session.Status == "closing" {
			err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: claim.ID, EnvironmentID: session.EnvironmentID, SessionID: session.ID})
		} else if CanStartContinuation(session) {
			_, err = CreateContinuation(ctx, tx, session, db.Computer{ID: sessionComputer.ID}, bindings)
		}
		if err != nil {
			return receipt, err
		}
	}
	return receipt, finishOperation(ctx, tx, claim, receipt)
}

// CompleteInterruption settles the held Turn after the caller verifies execution
// and owned-scope cleanup in the same transaction. The version records the last
// retained Computer head; process cleanup has its own evidence.
func CompleteInterruption(ctx context.Context, tx pgx.Tx, session db.Session, versionID pgtype.UUID, fingerprint string) error {
	q := db.New(tx)
	var sequence pgtype.Int8
	if session.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
		if err != nil {
			return err
		}
		if turn.Status != "running" || turn.Sequence != session.CommittedInputSequence+1 || !turn.InterruptRequestedAt.Valid {
			return &OperationError{Code: "stale_execution"}
		}
		if err := finishUnsettledMessages(ctx, q, session, turn.ID, "execution_interrupted"); err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"computer_disk_version_id": pgvalue.UUIDString(versionID), "hold_id": pgvalue.UUIDString(session.DispatchHoldID)})
		event, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgtype.UUID{}, "turn.interrupted", body, versionID)
		if err != nil {
			return err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turn.ID, Status: "interrupted", EventID: event.ID, Fingerprint: pgvalue.Text(fingerprint)}); err != nil {
			return err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	previousHold := session.DispatchHoldID
	held, err := q.CompleteSessionInterruption(ctx, db.CompleteSessionInterruptionParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, RunID: session.CurrentRunID, RunGeneration: session.RunGeneration, HoldID: session.DispatchHoldID, TurnID: session.ActiveTurnID, InputSequence: sequence, NewHoldID: pgvalue.UUID(uuid.NewV7())})
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"hold_id": pgvalue.UUIDString(held.DispatchHoldID), "previous_hold_id": pgvalue.UUIDString(previousHold), "reason": "interrupted", "computer_disk_version_id": pgvalue.UUIDString(versionID)})
	_, err = appendLifecycleEvent(ctx, q, held, pgtype.UUID{}, pgtype.UUID{}, "session.held", body, versionID)
	return err
}
