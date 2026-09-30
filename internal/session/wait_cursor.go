package session

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgtype"
)

// The cursor belongs to the request and its fingerprint. A wait references its
// admitted Turn; only a Computer checkpoint stores a suspended execution cursor.
func ValidateWaitCursor(execution run.Execution, wait db.RunWait, cursor pgtype.Int8) error {
	return validateWaitCursor(waitCursorScope{Run: execution.Run(), Attempt: execution.Attempt(), Session: execution.Session()}, wait, cursor)
}

// waitCursorScope is the part of a locked execution a wait cursor is checked
// against.
type waitCursorScope struct {
	Run     db.Run
	Attempt db.RunAttempt
	Session db.Session
}

func validateWaitCursor(a waitCursorScope, wait db.RunWait, cursor pgtype.Int8) error {
	if a.Run.EntrypointKind == "task" {
		if a.Run.SessionID.Valid || cursor.Valid || wait.TurnID.Valid {
			return ErrAuthority
		}
		return nil
	}
	if a.Run.EntrypointKind != "actor" || !a.Run.SessionID.Valid || a.Run.SessionID != a.Session.ID || a.Session.CurrentRunID != a.Run.ID || (a.Session.Status != "open" && a.Session.Status != "closing") {
		return ErrAuthority
	}
	if a.Session.DispatchHoldID.Valid || a.Session.CancelRequestedAt.Valid {
		return ErrTurnStopped
	}
	if wait.TurnID.Valid && (a.Session.ActiveTurnID != wait.TurnID || wait.TurnSessionID != a.Session.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != a.Session.RunGeneration) {
		return ErrTurnScope
	}
	if wait.Kind == db.WaitKindActorInput && wait.CompletedTurnID.Valid {
		if wait.CompletedTurnID != a.Session.ActiveTurnID || wait.TurnID != wait.CompletedTurnID {
			return ErrTurnScope
		}
	} else if a.Session.ActiveTurnID.Valid != wait.TurnID.Valid {
		return ErrTurnScope
	}
	want := a.Session.CommittedInputSequence
	if wait.TurnID.Valid && wait.Kind != db.WaitKindActorInput {
		want++
	}
	if !cursor.Valid || cursor.Int64 != want || !a.Attempt.SessionInputStartSequence.Valid || a.Attempt.SessionInputStartSequence.Int64 > a.Session.CommittedInputSequence || cursor.Int64 >= a.Session.NextInputSequence {
		return ErrAuthority
	}
	return nil
}
