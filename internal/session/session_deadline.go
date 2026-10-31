package session

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Deadline settlement uses server time after the ownership locks. It needs no
// live worker or guest; the process stop converges through Session control.
func expireSessionDeadline(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, env, session uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// Bound lock contention without imposing a total runtime ceiling on a
		// finite transaction. A timed-out attempt rolls back and remains due.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		if _, err := lockSession(ctx, tx, env, session); err != nil {
			return err
		}
		return expireSessionDeadlineTx(ctx, auditPolicy, tx, env, session)
	})
}

// TurnDeadlineElapsed is the interruption reason of a Turn that reached its
// maximum duration. Its process stops; queued Turns continue on a fresh one.
const TurnDeadlineElapsed = "turn_deadline_elapsed"

func expireSessionDeadlineTx(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, session uuid.UUID) error {
	var turn uuid.UUID
	var epoch int64
	err := tx.QueryRow(ctx, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
 WHERE environment_id=$1 AND session_id=$2 AND status='running' AND deadline_at<=clock_timestamp()
 RETURNING id,process_epoch`, env, session).Scan(&turn, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The stop deadline escalates a wedged harness to a counted failure.
	if err = requestProcessStop(ctx, tx, env, session, uuid.Nil(), epoch); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND status IN ('ready','starting') AND fenced_at IS NULL`, env, session, epoch); err != nil {
		return err
	}
	if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{session}); err != nil {
		return err
	}
	// An ordered save remains owned by persistence even after its Turn expires.
	// Deadline settlement never guesses whether capture or publication happened.
	if err := cancelTurnAsks(ctx, auditPolicy, tx, env, session, turn); err != nil {
		return err
	}
	if err := rejectTurnMessages(ctx, auditPolicy, tx, env, session, turn, true, "turn_terminated"); err != nil {
		return err
	}
	if err := auditServiceFact(ctx, tx, auditPolicy, audit.ControlPlane, audit.Event{EnvironmentID: env, SessionID: session, TurnID: turn, Action: "turn.interrupted", TargetKind: "turn", TargetID: turn, Details: audit.LifecycleDetails{Generation: epoch}}); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{TurnDeadlineElapsed})
	if err != nil {
		return err
	}
	return eventData(ctx, tx, env, session, turn, "turn.interrupted", data)
}
