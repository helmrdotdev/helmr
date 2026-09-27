package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestChildWaitScopeUsesSessionMembership(t *testing.T) {
	actorID, runID, computerID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	authority := run.ExecutionAuthority{Run: db.Run{ID: runID, ComputerID: computerID, SessionID: actorID, EntrypointKind: "actor"}, Session: db.Session{ID: actorID, CurrentRunID: runID, ComputerID: computerID, Status: "open", ActiveTurnID: turnID, RunGeneration: 3}}
	wait := db.RunWait{TurnID: turnID, TurnSessionID: actorID, TurnRunGeneration: pgtype.Int8{Int64: 3, Valid: true}}
	if err := validateChildWaitScope(authority, wait); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*run.ExecutionAuthority, *db.RunWait){
		func(a *run.ExecutionAuthority, w *db.RunWait) { a.Session.ComputerID = pgvalue.UUID(uuid.NewV7()) },
		func(a *run.ExecutionAuthority, w *db.RunWait) { a.Session.ID = pgvalue.UUID(uuid.NewV7()) },
		func(a *run.ExecutionAuthority, w *db.RunWait) { a.Session.DispatchHoldID = pgvalue.UUID(uuid.NewV7()) },
		func(a *run.ExecutionAuthority, w *db.RunWait) { a.Session.CurrentRunID = pgvalue.UUID(uuid.NewV7()) },
		func(a *run.ExecutionAuthority, w *db.RunWait) { w.TurnRunGeneration.Int64++ },
		func(a *run.ExecutionAuthority, w *db.RunWait) { w.TurnSessionID = pgvalue.UUID(uuid.NewV7()) },
		func(a *run.ExecutionAuthority, w *db.RunWait) { w.TurnID = pgtype.UUID{} },
	} {
		a, w := authority, wait
		mutate(&a, &w)
		if err := validateChildWaitScope(a, w); err == nil {
			t.Fatalf("accepted stale scope: %+v %+v", a.Session, w)
		}
	}
	authority.Session.ActiveTurnID = pgtype.UUID{}
	if err := validateChildWaitScope(authority, db.RunWait{}); err != nil {
		t.Fatalf("between turns: %v", err)
	}
	authority = run.ExecutionAuthority{Run: db.Run{EntrypointKind: "task"}}
	if err := validateChildWaitScope(authority, db.RunWait{}); err != nil {
		t.Fatal(err)
	}
	if err := validateChildWaitScope(authority, wait); err == nil {
		t.Fatal("Task accepted Actor Turn")
	}
}
