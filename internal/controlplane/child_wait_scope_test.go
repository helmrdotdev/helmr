package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestChildWaitScopeUsesSessionMembership(t *testing.T) {
	actorID, runID, computerID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	scope := childWaitScope{run: db.Run{ID: runID, ComputerID: computerID, SessionID: actorID, EntrypointKind: "actor"}, session: db.Session{ID: actorID, CurrentRunID: runID, ComputerID: computerID, Status: "open", ActiveTurnID: turnID, RunGeneration: 3}}
	wait := db.RunWait{TurnID: turnID, TurnSessionID: actorID, TurnRunGeneration: pgtype.Int8{Int64: 3, Valid: true}}
	if err := validateChildWaitScope(scope, wait); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*childWaitScope, *db.RunWait){
		func(a *childWaitScope, w *db.RunWait) { a.session.ComputerID = pgvalue.UUID(uuid.NewV7()) },
		func(a *childWaitScope, w *db.RunWait) { a.session.ID = pgvalue.UUID(uuid.NewV7()) },
		func(a *childWaitScope, w *db.RunWait) { a.session.DispatchHoldID = pgvalue.UUID(uuid.NewV7()) },
		func(a *childWaitScope, w *db.RunWait) { a.session.CurrentRunID = pgvalue.UUID(uuid.NewV7()) },
		func(a *childWaitScope, w *db.RunWait) { w.TurnRunGeneration.Int64++ },
		func(a *childWaitScope, w *db.RunWait) { w.TurnSessionID = pgvalue.UUID(uuid.NewV7()) },
		func(a *childWaitScope, w *db.RunWait) { w.TurnID = pgtype.UUID{} },
	} {
		a, w := scope, wait
		mutate(&a, &w)
		if err := validateChildWaitScope(a, w); err == nil {
			t.Fatalf("accepted stale scope: %+v %+v", a.session, w)
		}
	}
	scope.session.ActiveTurnID = pgtype.UUID{}
	if err := validateChildWaitScope(scope, db.RunWait{}); err != nil {
		t.Fatalf("between turns: %v", err)
	}
	scope = childWaitScope{run: db.Run{EntrypointKind: "task"}}
	if err := validateChildWaitScope(scope, db.RunWait{}); err != nil {
		t.Fatal(err)
	}
	if err := validateChildWaitScope(scope, wait); err == nil {
		t.Fatal("Task accepted Actor Turn")
	}
}
