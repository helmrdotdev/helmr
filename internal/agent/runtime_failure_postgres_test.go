package agent

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
)

func TestRuntimeFailurePreservesReceiptAndContinuesSession(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "failure")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	failure := TurnError{Code: "application_error", Message: "bad\x00input"}
	fail := func() (json.RawMessage, error) {
		return RuntimeFail(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, failure, "drained")
	}
	if _, err := fail(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unclosed failure: %v", err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	first, err := fail()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status string
		Error  TurnError
	}
	if err := json.Unmarshal(first, &got); err != nil || got.Status != "failed" || got.Error != failure {
		t.Fatalf("outcome %s: %v", first, err)
	}
	next := f.enqueue(t, "next")
	if dispatch, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil || dispatch.TurnID != next.TurnID {
		t.Fatalf("next dispatch: %+v %v", dispatch, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	replay, err := fail()
	if err != nil || string(replay) != string(first) {
		t.Fatalf("replay %s: %v", replay, err)
	}
	failure.Message = "different"
	if _, err := fail(); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed failure: %v", err)
	}
	var saves, holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_saves WHERE turn_id=$1),(SELECT count(*) FROM session_holds WHERE session_id=$2)`, a.TurnID, f.session).Scan(&saves, &holds); err != nil || saves != 0 || holds != 0 {
		t.Fatalf("failure created save/hold %d/%d: %v", saves, holds, err)
	}
}

func TestRuntimeFailureHonorsElapsedDeadline(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "deadline")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, a.TurnID)
	outcome, err := RuntimeFail(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, TurnError{Code: "error"}, "drained")
	if err != nil || string(outcome) != `{"status":"interrupted"}` {
		t.Fatalf("deadline outcome %s: %v", outcome, err)
	}
}
