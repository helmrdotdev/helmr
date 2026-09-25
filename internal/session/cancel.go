package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgtype"
)

// Cancel escalates closure without starting any remaining customer work. The
// caller acquires the owned Run graph before entering Session authority.
func Cancel(ctx context.Context, q db.Querier, request ControlRequest, graph run.OwnedFinalization) (ControlReceipt, error) {
	actor, err := lockSession(ctx, q, request.Target)
	if err != nil {
		return ControlReceipt{}, err
	}
	claim, err := claimOperation(ctx, q, request, "session.cancel", struct{}{})
	if err != nil {
		return ControlReceipt{}, err
	}
	var receipt ControlReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = ControlReceipt{ID: pgvalue.MustUUIDValue(claim.ID), SessionID: request.SessionID, Status: "accepted"}
	if actor.Status == "closed" {
		return receipt, finishOperation(ctx, q, claim, receipt)
	}
	if actor.Status != "open" && actor.Status != "closing" {
		receipt.Code = "session_not_open"
		return receipt, finishOperation(ctx, q, claim, receipt)
	}
	if !actor.CancelRequestedAt.Valid {
		actor, err = q.BeginSessionCancellation(ctx, db.BeginSessionCancellationParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return receipt, err
		}
		if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, "session.cancel_requested", []byte(`{}`), pgtype.UUID{}); err != nil {
			return receipt, err
		}
	}
	queued, err := q.LockQueuedSessionTurns(ctx, db.LockQueuedSessionTurnsParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
	if err != nil {
		return receipt, err
	}
	for _, turn := range queued {
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.cancelled", []byte(`{"reason":"session_cancelled"}`), pgtype.UUID{})
		if err != nil {
			return receipt, err
		}
		if _, err = q.CancelQueuedSessionTurn(ctx, db.CancelQueuedSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, EventID: event.ID}); err != nil {
			return receipt, err
		}
	}
	if actor.ActiveTurnID.Valid {
		if err = rejectQueuedMessages(ctx, q, actor, actor.ActiveTurnID, "session_cancelled"); err != nil {
			return receipt, err
		}
	}
	if actor.CurrentRunID.Valid {
		if !actor.DispatchHoldID.Valid {
			current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: actor.EnvironmentID, ID: actor.CurrentRunID})
			if err != nil {
				return receipt, err
			}
			if actor.ActiveTurnID.Valid {
				if _, err = q.RequestSessionTurnInterrupt(ctx, db.RequestSessionTurnInterruptParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: actor.ActiveTurnID}); err != nil {
					return receipt, err
				}
				if _, err = appendLifecycleEvent(ctx, q, actor, actor.ActiveTurnID, pgtype.UUID{}, "turn.interrupt_requested", []byte(`{"reason":"session_cancelled"}`), pgtype.UUID{}); err != nil {
					return receipt, err
				}
			}
			actor, err = run.HoldSessionExecution(ctx, q, actor, current.CurrentAttemptNumber, "interrupt_requested")
			if err != nil {
				return receipt, err
			}
		}
		// Existing loss/hold authority is preserved. This requests physical stop;
		// it does not convert uncertainty into successful interruption.
		if _, err = graph.RequestHeldActorStop(ctx, pgvalue.MustUUIDValue(actor.DispatchHoldID)); err != nil {
			return receipt, err
		}
	}
	err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
	if err != nil {
		return receipt, err
	}
	return receipt, finishOperation(ctx, q, claim, receipt)
}
