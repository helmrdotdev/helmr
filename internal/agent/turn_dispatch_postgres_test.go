package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestTurnDispatchRetainsAdmissionTimeAndExactOrigin(t *testing.T) {
	f := newFixture(t)
	origin := f.enqueue(t, "origin")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	peer := f.peer(t)
	caller := Caller{Kind: "session", ID: f.session, TurnID: origin.TurnID, Execution: f.execution(), Host: f.host()}
	admission, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "peer-input", Input: json.RawMessage(`[{"type":"text","text":"{\"message\":\"retained\"}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := Dispatch(t.Context(), f.pool, peer.execution())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := Dispatch(t.Context(), f.pool, peer.execution())
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("dispatch changed on replay: %+v %+v %v", first, replay, err)
	}
	if first.TurnID != admission.TurnID || first.Sequence != admission.Sequence || first.CreatedAt.IsZero() || first.Source.Kind != "session" || first.Source.RequesterSessionID == nil || *first.Source.RequesterSessionID != f.session || first.Source.OriginTurnID == nil || *first.Source.OriginTurnID != origin.TurnID {
		t.Fatalf("lost exact admission metadata: %+v", first)
	}
	var value []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(first.Input, &value); err != nil || len(value) != 1 || value[0].Type != "text" || value[0].Text != `{"message":"retained"}` {
		t.Fatalf("lost input: %s %v", first.Input, err)
	}
	// The source is a typed Session/Turn relationship, not an unconstrained UUID.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET origin_turn_id=NULL WHERE environment_id=$1 AND id=$2`, f.env, admission.TurnID)
	if _, err := f.pool.Exec(t.Context(), `UPDATE turns SET origin_turn_id=id WHERE environment_id=$1 AND id=$2`, f.env, admission.TurnID); err == nil {
		t.Fatal("cross-Session origin accepted")
	}
}
