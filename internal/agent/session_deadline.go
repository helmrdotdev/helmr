package agent

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Deadline settlement uses server time after the ownership locks. It needs no
// live worker or guest; native stop converges separately through Session control.
func expireSessionDeadline(ctx context.Context, pool db.TxBeginner, env, session uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// Bound lock contention without imposing a total runtime ceiling on a
		// finite transaction. A timed-out attempt rolls back and remains due.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		if _, err := lockSession(ctx, tx, env, session); err != nil {
			return err
		}
		return expireSessionDeadlineTx(ctx, tx, env, session)
	})
}

func expireSessionDeadlineTx(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) error {
	var turn uuid.UUID
	var epoch int64
	err := tx.QueryRow(ctx, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
 WHERE environment_id=$1 AND session_id=$2 AND status IN ('running','finalizing') AND deadline_at<=clock_timestamp()
 RETURNING id,process_epoch`, env, session).Scan(&turn, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	hold := uuid.NewV7()
	if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','Turn deadline elapsed')`, env, hold, session); err != nil {
		return err
	}
	if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{session}); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND status IN ('ready','starting') AND fenced_at IS NULL`, env, session, epoch); err != nil {
		return err
	}
	// An ordered save remains owned by persistence even after its Turn expires.
	// Deadline settlement never guesses whether capture or publication happened.
	data, err := json.Marshal(struct {
		HoldID uuid.UUID `json:"holdId"`
	}{hold})
	if err != nil {
		return err
	}
	if err = eventData(ctx, tx, env, session, turn, "session.deadline", data); err != nil {
		return err
	}
	if err := cancelTurnAsks(ctx, tx, env, session, turn); err != nil {
		return err
	}
	if err := rejectTurnMessages(ctx, tx, env, session, turn, true, "turn_terminated"); err != nil {
		return err
	}
	return event(ctx, tx, env, session, turn, "turn.interrupted")
}
