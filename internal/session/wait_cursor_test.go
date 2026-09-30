package session

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerWaitCursorCompletedActorInputReplay(t *testing.T) {
	sessionID, runID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	a := waitCursorScope{
		run:     db.Run{ID: runID, EntrypointKind: "actor", SessionID: sessionID},
		session: db.Session{ID: sessionID, CurrentRunID: runID, Status: "open", CommittedInputSequence: 3, NextInputSequence: 5, ActiveTurnID: turnID, RunGeneration: 2},
		attempt: db.RunAttempt{SessionInputStartSequence: pgtype.Int8{Int64: 1, Valid: true}},
	}
	receipt := db.RunWait{Kind: db.WaitKindActorInput, CompletedTurnID: turnID, TurnID: turnID, TurnSessionID: sessionID, TurnRunGeneration: pgtype.Int8{Int64: 2, Valid: true}}
	cursor := pgtype.Int8{Int64: 3, Valid: true}
	if err := validateWaitCursor(a, receipt, cursor); err != nil {
		t.Fatalf("completed receive replay rejected: %v", err)
	}
	if err := validateWaitCursor(a, db.RunWait{Kind: db.WaitKindActorInput}, cursor); err == nil {
		t.Fatal("new receive accepted during active Turn")
	}
	receipt.CompletedTurnID = pgvalue.UUID(uuid.NewV7())
	if err := validateWaitCursor(a, receipt, cursor); err == nil {
		t.Fatal("unrelated completed Turn accepted")
	}
	receipt.CompletedTurnID = turnID
	cursor.Int64 = 4
	if err := validateWaitCursor(a, receipt, cursor); err == nil {
		t.Fatal("receive replay incremented its original cursor")
	}
}
