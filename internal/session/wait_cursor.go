package session

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgtype"
)

// The cursor belongs to the request and its fingerprint. A wait references its
// admitted Turn; only a Computer checkpoint stores a suspended execution cursor.
func ValidateWaitCursor(execution run.Execution, wait db.RunWait, cursor pgtype.Int8) error {
	return validateWaitCursor(waitCursorScope{run: execution.Run(), attempt: execution.Attempt(), session: execution.Session()}, wait, cursor)
}

// waitCursorScope is the part of a locked execution a wait cursor is checked
// against.
type waitCursorScope struct {
	run     db.Run
	attempt db.RunAttempt
	session db.Session
}

func validateWaitCursor(a waitCursorScope, wait db.RunWait, cursor pgtype.Int8) error {
	if a.run.EntrypointKind == "task" {
		if a.run.SessionID.Valid || cursor.Valid || wait.TurnID.Valid {
			return ErrAuthority
		}
		return nil
	}
	if a.run.EntrypointKind != "actor" || !a.run.SessionID.Valid || a.run.SessionID != a.session.ID || a.session.CurrentRunID != a.run.ID || (a.session.Status != "open" && a.session.Status != "closing") {
		return ErrAuthority
	}
	if a.session.DispatchHoldID.Valid || a.session.CancelRequestedAt.Valid {
		return ErrTurnStopped
	}
	if wait.TurnID.Valid && (a.session.ActiveTurnID != wait.TurnID || wait.TurnSessionID != a.session.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != a.session.RunGeneration) {
		return ErrTurnScope
	}
	if wait.Kind == db.WaitKindActorInput && wait.CompletedTurnID.Valid {
		if wait.CompletedTurnID != a.session.ActiveTurnID || wait.TurnID != wait.CompletedTurnID {
			return ErrTurnScope
		}
	} else if a.session.ActiveTurnID.Valid != wait.TurnID.Valid {
		return ErrTurnScope
	}
	want := a.session.CommittedInputSequence
	if wait.TurnID.Valid && wait.Kind != db.WaitKindActorInput {
		want++
	}
	if !cursor.Valid || cursor.Int64 != want || !a.attempt.SessionInputStartSequence.Valid || a.attempt.SessionInputStartSequence.Int64 > a.session.CommittedInputSequence || cursor.Int64 >= a.session.NextInputSequence {
		return ErrAuthority
	}
	return nil
}
