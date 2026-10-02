package session

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// resolveInputWait settles one receive under the caller's locked Computer,
// Session, Run, Attempt and Wait authority. Accepted FIFO input precedes drained
// close, which precedes timeout. The transaction commits both Turn activation
// and delivery; a terminal wait never changes its result.
func resolveInputWait(ctx context.Context, tx pgx.Tx, session db.Session, wait db.RunWait) (db.RunWait, error) {
	if wait.ConditionStatus != "pending" {
		return wait, nil
	}
	if session.CancelRequestedAt.Valid || session.DispatchHoldID.Valid || session.ActiveTurnID.Valid ||
		(session.Status != "open" && session.Status != "closing") || session.CurrentRunID != wait.RunID {
		return wait, nil
	}
	if wait.Kind != db.WaitKindSessionInput || wait.SessionID != session.ID ||
		!wait.AfterInputSequence.Valid || wait.AfterInputSequence.Int64 != session.CommittedInputSequence {
		return db.RunWait{}, ErrAuthority
	}
	q := db.New(tx)
	turn, err := q.GetSessionTurnAtSequenceForUpdate(ctx, db.GetSessionTurnAtSequenceForUpdateParams{
		EnvironmentID: session.EnvironmentID, SessionID: session.ID,
		Sequence: wait.AfterInputSequence.Int64 + 1,
	})
	if err == nil {
		return CompleteWait(ctx, tx, wait, turn)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.RunWait{}, err
	}
	if session.CommittedInputSequence < session.NextInputSequence-1 {
		return db.RunWait{}, ErrAuthority
	}
	if session.Status == "closing" && session.CloseSequence.Valid &&
		session.CommittedInputSequence >= session.CloseSequence.Int64 {
		return FailWait(ctx, tx, wait, "session_closed")
	}
	if wait.TimeoutAt.Valid {
		now, err := q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			return db.RunWait{}, err
		}
		if !now.Valid {
			return db.RunWait{}, ErrAuthority
		}
		if !now.Time.Before(wait.TimeoutAt.Time) {
			return FailWait(ctx, tx, wait, "wait_timeout")
		}
	}
	return wait, nil
}
