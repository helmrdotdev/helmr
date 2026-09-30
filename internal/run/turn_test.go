package run

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerWaitCursorCompletedActorInputReplay(t *testing.T) {
	sessionID, runID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	a := Execution{
		run:     db.Run{ID: runID, EntrypointKind: "actor", SessionID: sessionID},
		session: db.Session{ID: sessionID, CurrentRunID: runID, Status: "open", CommittedInputSequence: 3, NextInputSequence: 5, ActiveTurnID: turnID, RunGeneration: 2},
		attempt: db.RunAttempt{SessionInputStartSequence: pgtype.Int8{Int64: 1, Valid: true}},
	}
	receipt := db.RunWait{Kind: db.WaitKindActorInput, CompletedTurnID: turnID, TurnID: turnID, TurnSessionID: sessionID, TurnRunGeneration: pgtype.Int8{Int64: 2, Valid: true}}
	cursor := pgtype.Int8{Int64: 3, Valid: true}
	if err := a.ValidateWaitCursor(receipt, cursor); err != nil {
		t.Fatalf("completed receive replay rejected: %v", err)
	}
	if err := a.ValidateWaitCursor(db.RunWait{Kind: db.WaitKindActorInput}, cursor); err == nil {
		t.Fatal("new receive accepted during active Turn")
	}
	receipt.CompletedTurnID = pgvalue.UUID(uuid.NewV7())
	if err := a.ValidateWaitCursor(receipt, cursor); err == nil {
		t.Fatal("unrelated completed Turn accepted")
	}
	receipt.CompletedTurnID = turnID
	cursor.Int64 = 4
	if err := a.ValidateWaitCursor(receipt, cursor); err == nil {
		t.Fatal("receive replay incremented its original cursor")
	}
}

func TestWaitCursorOutcomes(t *testing.T) {
	sessionID, runID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	actor := func() Execution {
		return Execution{
			run:     db.Run{ID: runID, EntrypointKind: "actor", SessionID: sessionID},
			session: db.Session{ID: sessionID, CurrentRunID: runID, Status: "open", CommittedInputSequence: 3, NextInputSequence: 5, RunGeneration: 2},
			attempt: db.RunAttempt{SessionInputStartSequence: pgtype.Int8{Int64: 1, Valid: true}},
		}
	}
	cursor := pgtype.Int8{Int64: 3, Valid: true}
	for name, test := range map[string]struct {
		mutate func(*Execution, *db.RunWait, *pgtype.Int8)
		want   error
	}{
		"idle actor": {func(*Execution, *db.RunWait, *pgtype.Int8) {}, nil},
		"task without cursor": {func(e *Execution, _ *db.RunWait, c *pgtype.Int8) {
			*e = Execution{run: db.Run{EntrypointKind: "task"}}
			*c = pgtype.Int8{}
		}, nil},
		"task with cursor":    {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { *e = Execution{run: db.Run{EntrypointKind: "task"}} }, ErrWaitCursor},
		"other Session's Run": {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { e.session.CurrentRunID = pgvalue.UUID(uuid.NewV7()) }, ErrWaitCursor},
		"closed Session":      {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { e.session.Status = "closed" }, ErrWaitCursor},
		"held Session":        {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { e.session.DispatchHoldID = turnID }, ErrTurnStopped},
		"cancelled Session": {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) {
			e.session.CancelRequestedAt = pgtype.Timestamptz{Valid: true}
		}, ErrTurnStopped},
		"active Turn without it": {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { e.session.ActiveTurnID = turnID }, ErrTurnScope},
		"stale Turn generation": {func(e *Execution, w *db.RunWait, _ *pgtype.Int8) {
			e.session.ActiveTurnID = turnID
			*w = db.RunWait{TurnID: turnID, TurnSessionID: sessionID, TurnRunGeneration: pgtype.Int8{Int64: 1, Valid: true}}
		}, ErrTurnScope},
		"missing cursor":        {func(_ *Execution, _ *db.RunWait, c *pgtype.Int8) { *c = pgtype.Int8{} }, ErrWaitCursor},
		"cursor ahead of input": {func(_ *Execution, _ *db.RunWait, c *pgtype.Int8) { c.Int64 = 4 }, ErrWaitCursor},
		"attempt started later": {func(e *Execution, _ *db.RunWait, _ *pgtype.Int8) { e.attempt.SessionInputStartSequence.Int64 = 4 }, ErrWaitCursor},
	} {
		t.Run(name, func(t *testing.T) {
			execution, wait, c := actor(), db.RunWait{}, cursor
			test.mutate(&execution, &wait, &c)
			if err := execution.ValidateWaitCursor(wait, c); err != test.want {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestLockedTurnOutcomes(t *testing.T) {
	sessionID, runID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	scope := TurnScope{EnvironmentID: uuid.NewV7(), SessionID: pgvalue.MustUUIDValue(sessionID), TurnID: pgvalue.MustUUIDValue(turnID), RunID: pgvalue.MustUUIDValue(runID), AttemptNumber: 1, RunGeneration: 2}
	running := func() LockedTurn {
		return LockedTurn{
			session: db.Session{ID: sessionID, CurrentRunID: runID, RunGeneration: 2, ActiveTurnID: turnID, Status: "open"},
			turn:    db.SessionTurn{ID: turnID, Status: "running", RunID: runID, AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, RunGeneration: pgtype.Int8{Int64: 2, Valid: true}},
			scope:   scope,
		}
	}
	if _, err := LockTurn(t.Context(), nil, TurnScope{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID}); err != ErrTurnScope {
		t.Fatalf("unaddressed Turn=%v", err)
	}
	for name, test := range map[string]struct {
		mutate   func(*LockedTurn)
		want     error
		wantWork error
	}{
		"running":           {func(*LockedTurn) {}, nil, nil},
		"closing":           {func(l *LockedTurn) { l.session.Status = "closing" }, nil, nil},
		"settling":          {func(l *LockedTurn) { l.turn.SettlementStartedAt = pgtype.Timestamptz{Valid: true} }, nil, ErrTurnUnsettled},
		"other attempt":     {func(l *LockedTurn) { l.scope.AttemptNumber = 2 }, ErrTurnScope, ErrTurnScope},
		"other generation":  {func(l *LockedTurn) { l.session.RunGeneration = 3 }, ErrTurnScope, ErrTurnScope},
		"other current Run": {func(l *LockedTurn) { l.session.CurrentRunID = pgvalue.UUID(uuid.NewV7()) }, ErrTurnScope, ErrTurnScope},
		"inactive Turn":     {func(l *LockedTurn) { l.session.ActiveTurnID = pgtype.UUID{} }, ErrTurnNotActive, ErrTurnNotActive},
		"closed Session":    {func(l *LockedTurn) { l.session.Status = "closed" }, ErrTurnNotActive, ErrTurnNotActive},
		"held Session":      {func(l *LockedTurn) { l.session.DispatchHoldID = turnID }, ErrTurnStopped, ErrTurnStopped},
		"interrupted Turn":  {func(l *LockedTurn) { l.turn.InterruptRequestedAt = pgtype.Timestamptz{Valid: true} }, ErrTurnStopped, ErrTurnStopped},
	} {
		t.Run(name, func(t *testing.T) {
			locked := running()
			test.mutate(&locked)
			turn, err := locked.Validate()
			if err != test.want || turn.ID != turnID {
				t.Fatalf("Validate turn=%s error=%v want %v", pgvalue.UUIDString(turn.ID), err, test.want)
			}
			if _, err = locked.ValidateWork(); err != test.wantWork {
				t.Fatalf("ValidateWork error=%v want %v", err, test.wantWork)
			}
		})
	}
}
