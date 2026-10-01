package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Cancel escalates closure without starting any remaining customer work. The
// caller acquires the owned Run graph before entering Session authority.
func Cancel(ctx context.Context, tx pgx.Tx, request ControlRequest, graph run.OwnedFinalization) (ControlReceipt, error) {
	q := db.New(tx)
	session, err := lockSession(ctx, tx, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	claim, err := claimOperation(ctx, tx, request, "session.cancel", struct{}{})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, Status: "accepted"}
	if session.Status == "closed" {
		return receipt, finishOperation(ctx, tx, claim, receipt)
	}
	if session.Status != "open" && session.Status != "closing" {
		receipt.Code = "session_not_open"
		return receipt, finishOperation(ctx, tx, claim, receipt)
	}
	if !session.CancelRequestedAt.Valid {
		session, err = q.BeginSessionCancellation(ctx, db.BeginSessionCancellationParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		if err != nil {
			return receipt, err
		}
		if _, err = appendLifecycleEvent(ctx, q, session, pgtype.UUID{}, pgtype.UUID{}, "session.cancel_requested", []byte(`{}`), pgtype.UUID{}); err != nil {
			return receipt, err
		}
	}
	queued, err := q.LockQueuedSessionTurns(ctx, db.LockQueuedSessionTurnsParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID})
	if err != nil {
		return receipt, err
	}
	for _, turn := range queued {
		event, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgtype.UUID{}, "turn.cancelled", []byte(`{"reason":"session_cancelled"}`), pgtype.UUID{})
		if err != nil {
			return receipt, err
		}
		if _, err = q.CancelQueuedSessionTurn(ctx, db.CancelQueuedSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turn.ID, EventID: event.ID}); err != nil {
			return receipt, err
		}
	}
	if session.ActiveTurnID.Valid {
		if err = rejectQueuedMessages(ctx, q, session, session.ActiveTurnID, "session_cancelled"); err != nil {
			return receipt, err
		}
	}
	if session.CurrentRunID.Valid {
		if !session.DispatchHoldID.Valid {
			current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: session.EnvironmentID, ID: session.CurrentRunID})
			if err != nil {
				return receipt, err
			}
			if session.ActiveTurnID.Valid {
				if _, err = q.RequestSessionTurnInterrupt(ctx, db.RequestSessionTurnInterruptParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: session.ActiveTurnID}); err != nil {
					return receipt, err
				}
				if _, err = appendLifecycleEvent(ctx, q, session, session.ActiveTurnID, pgtype.UUID{}, "turn.interrupt_requested", []byte(`{"reason":"session_cancelled"}`), pgtype.UUID{}); err != nil {
					return receipt, err
				}
			}
			session, err = run.HoldSessionExecution(ctx, q, session, current.CurrentAttemptNumber, "interrupt_requested")
			if err != nil {
				return receipt, err
			}
		}
		// Existing loss/hold authority is preserved. This requests physical stop;
		// it does not convert uncertainty into successful interruption.
		if _, err = graph.RequestHeldSessionStop(ctx, pgvalue.MustUUIDValue(session.DispatchHoldID)); err != nil {
			return receipt, err
		}
	}
	err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: session.EnvironmentID, SessionID: session.ID})
	if err != nil {
		return receipt, err
	}
	return receipt, finishOperation(ctx, tx, claim, receipt)
}
