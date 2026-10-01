package run

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestChildWaitScopeUsesSessionMembership(t *testing.T) {
	actorID, runID, computerID, turnID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	scope := Execution{run: db.Run{ID: runID, ComputerID: computerID, SessionID: actorID, EntrypointKind: "actor"}, session: db.Session{ID: actorID, CurrentRunID: runID, ComputerID: computerID, Status: "open", ActiveTurnID: turnID, RunGeneration: 3}}
	wait := db.RunWait{TurnID: turnID, TurnSessionID: actorID, TurnRunGeneration: pgtype.Int8{Int64: 3, Valid: true}}
	if err := scope.validateChildWaitScope(wait); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Execution, *db.RunWait){
		func(a *Execution, w *db.RunWait) { a.session.ComputerID = pgvalue.UUID(uuid.NewV7()) },
		func(a *Execution, w *db.RunWait) { a.session.ID = pgvalue.UUID(uuid.NewV7()) },
		func(a *Execution, w *db.RunWait) { a.session.DispatchHoldID = pgvalue.UUID(uuid.NewV7()) },
		func(a *Execution, w *db.RunWait) { a.session.CurrentRunID = pgvalue.UUID(uuid.NewV7()) },
		func(a *Execution, w *db.RunWait) { w.TurnRunGeneration.Int64++ },
		func(a *Execution, w *db.RunWait) { w.TurnSessionID = pgvalue.UUID(uuid.NewV7()) },
		func(a *Execution, w *db.RunWait) { w.TurnID = pgtype.UUID{} },
	} {
		a, w := scope, wait
		mutate(&a, &w)
		if err := a.validateChildWaitScope(w); err == nil {
			t.Fatalf("accepted stale scope: %+v %+v", a.session, w)
		}
	}
	scope.session.ActiveTurnID = pgtype.UUID{}
	if err := scope.validateChildWaitScope(db.RunWait{}); err != nil {
		t.Fatalf("between turns: %v", err)
	}
	scope = Execution{run: db.Run{EntrypointKind: "task"}}
	if err := scope.validateChildWaitScope(db.RunWait{}); err != nil {
		t.Fatal(err)
	}
	if err := scope.validateChildWaitScope(wait); err == nil {
		t.Fatal("Task accepted Actor Turn")
	}
}

func TestParseWaitTurnRequiresACompleteTurn(t *testing.T) {
	turnID := uuid.NewV7().String()
	generation := int64(2)
	zero := int64(0)
	malformed := "turn"
	if id, gen, err := ParseWaitTurn(nil, nil); err != nil || id.Valid || gen.Valid {
		t.Fatalf("no Turn = %v %v %v", id, gen, err)
	}
	if id, gen, err := ParseWaitTurn(&turnID, &generation); err != nil || pgvalue.UUIDString(id) != turnID || gen != (pgtype.Int8{Int64: 2, Valid: true}) {
		t.Fatalf("Turn = %v %v %v", id, gen, err)
	}
	for name, test := range map[string]struct {
		id         *string
		generation *int64
	}{"missing generation": {&turnID, nil}, "missing id": {nil, &generation}, "zero generation": {&turnID, &zero}} {
		if _, _, err := ParseWaitTurn(test.id, test.generation); !errors.Is(err, ErrTurnScope) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if _, _, err := ParseWaitTurn(&malformed, &generation); err == nil || err.Error() != "turn_id must be a canonical UUIDv7" {
		t.Fatalf("malformed Turn error = %v", err)
	}
}

func TestDecodeChildTaskReceiptRequiresCanonicalAuthority(t *testing.T) {
	runID, computerID := uuid.NewV7(), uuid.NewV7()
	receipt, err := decodeChildTaskReceipt([]byte(`{"runId":"` + runID.String() + `","computerId":"` + computerID.String() + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID != runID.String() || receipt.ComputerID != computerID.String() {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, raw := range []string{
		``,
		`{"runId":"` + runID.String() + `","computerId":"00000000-0000-0000-0000-000000000000"}`,
		`{"runId":"` + runID.String() + `","computerId":"` + computerID.String() + `","extra":1}`,
		`{"runId":"` + runID.String() + `","computerId":"` + computerID.String() + `"} {}`,
	} {
		if _, err := decodeChildTaskReceipt([]byte(raw)); !errors.Is(err, ErrTaskStartReceiptInvalid) {
			t.Fatalf("receipt %q error = %v", raw, err)
		}
	}
}

// The child call wait fingerprint is part of each registered wait's replay
// identity, so its digest must not drift.
func TestChildCallWaitFingerprintIsStable(t *testing.T) {
	got, err := RequestFingerprint("worker.child-call.wait", struct {
		Claim      string
		WaitID     string
		AttachID   string
		TurnID     string
		Generation int64
	}{"0a0b", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", "", 0})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:1181a137a808ec726f4494171a2328ff54f0c7560460a7e0f1dbeccfdcb3df78"; got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}
