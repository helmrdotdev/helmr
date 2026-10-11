package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestUniformTextInputAdmission(t *testing.T) {
	f := newAdmissionFixture(t)
	req := f.startRequest("message")
	req.Input = json.RawMessage(`[{"type":"text","text":"a\u0000雪"}]`)
	key := "conversation"
	req.SessionKey = &key
	first, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	view, err := GetTurn(t.Context(), f.pool, f.caller(), f.env, first.SessionID, first.TurnID)
	canonical, _ := conversation.Input(req.Input)
	if err != nil || string(view.Input) != string(canonical) {
		t.Fatalf("input changed %s: %v", view.Input, err)
	}
	// Promotion does not relax the input contract of a retained Session.
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,'sha256:'||repeat('a',64))`, f.env, other)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.env, other)
	bad := json.RawMessage(`[{"type":"json","value":null}]`)
	req.Input = bad
	req.RetryKey = "json"
	if _, err := Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, conversation.ErrUnsupported) {
		t.Fatalf("existing start ignored pin: %v", err)
	}
	if _, err := Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, Input: bad}); !errors.Is(err, conversation.ErrUnsupported) {
		t.Fatalf("enqueue ignored pin: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE session_id=$1`, first.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejection admitted work %d: %v", count, err)
	}
	for _, invalid := range []string{`{"type":"job","content":12}`, `{"type":"message","content":12}`, `null`, `"hello"`} {
		if _, err := Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, Input: json.RawMessage(invalid)}); !errors.Is(err, conversation.ErrInvalid) {
			t.Fatalf("accepted non-content input %s: %v", invalid, err)
		}
	}

}

func TestScheduledTextInputRetainsBytes(t *testing.T) {
	f := newAdmissionFixture(t)
	id := seedSchedule(t, f, time.Now().UTC().Truncate(time.Minute))
	input := json.RawMessage(`[{"type":"text","text":"scheduled\u0000"}]`)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_schedules SET input=$2 WHERE id=$1`, id, input)
	if err := EvaluateSchedule(t.Context(), f.pool, nil, f.env, id); err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT t.input FROM turns t JOIN agent_schedule_occurrences o ON o.turn_id=t.id WHERE o.schedule_id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	canonical, _ := conversation.Input(input)
	if string(got) != string(canonical) {
		t.Fatalf("schedule input changed %s", got)
	}
}
