package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRetainedWorkReads(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	first := f.enqueue(t, "first")
	second, err := Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "second", Input: json.RawMessage(`[{"type":"text","text":"{\"retained\":[1,\"two\",null]}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.f.secrets.Revoke(t.Context(), f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	page, err := ListTurns(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 1})
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ID != first.TurnID || page.NextSequence != first.Sequence {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = ListTurns(t.Context(), f.pool, f.caller(), TurnListRequest{EnvironmentID: f.env, SessionID: f.session, After: page.NextSequence, Limit: 1})
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ID != second.TurnID || page.NextSequence != 0 {
		t.Fatalf("next page: %+v %v", page, err)
	}
	retained, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, second.TurnID)
	if err != nil || retained.Status != "queued" || len(retained.Result) != 0 {
		t.Fatalf("retained: %+v %v", retained, err)
	}
	want, _ := digestJSON(json.RawMessage(`[{"type":"text","text":"{\"retained\":[1,\"two\",null]}"}]`))
	got, _ := digestJSON(retained.Input)
	if got != want {
		t.Fatalf("input changed: %s", retained.Input)
	}
	if _, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, uuid.NewV7(), second.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign Session: %v", err)
	}
	session, err := GetSession(t.Context(), f.pool, f.caller(), f.env, f.session)
	if err != nil || session.Status != "open" || session.ComputerID != f.computer {
		t.Fatalf("session: %+v %v", session, err)
	}
	holdA, holdB := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$4,'local','first'),($1,$3,$4,'subtree','second')`, f.env, holdA, holdB, f.session)
	session, err = GetSession(t.Context(), f.pool, f.caller(), f.env, f.session)
	if err != nil || len(session.Holds) != 2 {
		t.Fatalf("independent holds: %+v %v", session, err)
	}
	for i, hold := range session.Holds {
		if hold.ID != []uuid.UUID{holdA, holdB}[i] || hold.SessionID != f.session || hold.CreatedAt.IsZero() {
			t.Fatalf("hold identity or timestamp lost: %+v", hold)
		}
	}
	if _, err = ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	retained, err = GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, second.TurnID)
	if err != nil || retained.Status != "cancelled" || retained.TerminalAt == nil {
		t.Fatalf("cancelled: %+v %v", retained, err)
	}
	got, _ = digestJSON(retained.Input)
	if got != want {
		t.Fatalf("cancel lost input: %s", retained.Input)
	}
	sessions, err := ListSessions(t.Context(), f.pool, f.caller(), SessionListRequest{EnvironmentID: f.env, Limit: 100})
	if err != nil || len(sessions.Sessions) == 0 {
		t.Fatalf("sessions: %+v %v", sessions, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.caller().ID)
	if _, err := GetSession(t.Context(), f.pool, f.caller(), f.env, f.session); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale user: %v", err)
	}
}

func TestFinalizingReadDoesNotPublishResult(t *testing.T) {
	f := newFixture(t)
	admission, _ := f.finalize(t, "prepared")
	turn, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, admission.TurnID)
	if err != nil || turn.Status != "finalizing" || len(turn.Result) != 0 || turn.CompletionSaveID != nil {
		t.Fatalf("provisional result escaped: %+v %v", turn, err)
	}
}
