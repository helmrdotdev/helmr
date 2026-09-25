package session

import (
	"context"
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// FailExecution terminalizes an excluded Actor execution in the caller's
// transaction. The caller owns Computer disposition; this settles Session history.
func FailExecution(ctx context.Context, q db.Querier, actor db.Session, failure json.RawMessage, fingerprint string, completedAt pgtype.Timestamptz) error {
	terminalEvent, queueReason := "session.failed", "session_failed"
	queuedBody, err := json.Marshal(map[string]string{"reason": queueReason})
	if err != nil {
		return err
	}
	var sequence pgtype.Int8
	if actor.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
		if err != nil {
			return err
		}
		if turn.Status != "running" || turn.Sequence != actor.CommittedInputSequence+1 {
			return ErrAuthority
		}
		if err = finishUnsettledMessages(ctx, q, actor, turn.ID, "session_failed"); err != nil {
			return err
		}
		body, err := json.Marshal(map[string]any{"error": failure})
		if err != nil {
			return err
		}
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.failed", body, pgtype.UUID{})
		if err != nil {
			return err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, Status: "failed", EventID: event.ID, Fingerprint: pgtype.Text{String: fingerprint, Valid: true}}); err != nil {
			return err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	queued, err := q.LockQueuedSessionTurns(ctx, db.LockQueuedSessionTurnsParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID})
	if err != nil {
		return err
	}
	for _, turn := range queued {
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.cancelled", queuedBody, pgtype.UUID{})
		if err != nil {
			return err
		}
		if _, err = q.CancelQueuedSessionTurn(ctx, db.CancelQueuedSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, EventID: event.ID}); err != nil {
			return err
		}
	}
	body, err := json.Marshal(map[string]any{"error": failure})
	if err != nil {
		return err
	}
	if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, terminalEvent, body, pgtype.UUID{}); err != nil {
		return err
	}
	_, err = q.FailActorSession(ctx, db.FailActorSessionParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID, RunGeneration: actor.RunGeneration, Failure: failure, CompletedAt: completedAt, InputSequence: sequence})
	return err
}
