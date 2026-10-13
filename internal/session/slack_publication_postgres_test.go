package session_test

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slack"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackRevokedSourceCannotOpenReplacementRoot(t *testing.T) {
	for _, kind := range []string{"publication", "authorization"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdmissionFixture(t)
			installation, channel := slackRouteFixture(t, f)
			bindFixtureSlackRoot(t, f, channel)
			parent := f.enqueue(t, "parent")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			if kind == "publication" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
			} else {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, installation)
			}
			caller := session.Caller{Kind: "session", ID: f.session, TurnID: parent.TurnID, Execution: f.execution(), Host: f.host()}
			req := f.startRequest("independent")
			req.ComputerID = f.computer
			if _, err := slack.Start(t.Context(), f.pool, nil, nil, caller, req); !errors.Is(err, slack.ErrSourceConversationUnavailable) {
				t.Fatal(err)
			}
			req.IdempotencyKey = "child"
			child, err := session.Spawn(t.Context(), f.pool, nil, caller, req)
			if err != nil {
				t.Fatal(err)
			}
			c := f
			c.session = child.SessionID
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,attachment_sequence,control_sequence,control_attachment,control_kind,control_generation,control_acknowledged_at) VALUES($1,$2,1,$3,1,'ready',1,1,1,'resume',1,clock_timestamp())`, f.env, c.session, f.computer)
			if _, err = dispatchTurnForTest(t.Context(), f.pool, c.execution()); err != nil {
				t.Fatal(err)
			}
			if _, err = session.RuntimeOutput(t.Context(), f.pool, *c.host(), c.execution(), child.TurnID, uuid.NewV7(), json.RawMessage(`"child output"`)); err != nil {
				t.Fatal(err)
			}
			var exact bool
			if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_threads)=1 AND NOT EXISTS(SELECT 1 FROM slack_threads own WHERE own.front_session_id=s.id) AND source.thread_id=root.id AND EXISTS(SELECT 1 FROM session_events e WHERE e.session_id=s.id AND e.kind='slack.delivery_unavailable') FROM sessions s JOIN slack_thread_sources source ON source.session_id=s.id JOIN slack_threads root ON root.front_session_id=$2 WHERE s.id=$1`, child.SessionID, f.session).Scan(&exact); err != nil || !exact {
				t.Fatal(exact, err)
			}
		})
	}
}

func TestSlackRuntimeContentPreservesAdmissionRoute(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "internal", true: "slack"}[routed], func(t *testing.T) {
			f := newFixture(t)
			_, channel := slackRouteFixture(t, f)
			if routed {
				bindFixtureSlackRoot(t, f, channel)
			}
			turn := f.enqueue(t, "first")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{`[]`, `[{"type":"text","text":""}]`, `[{"type":"text","text":"progress"}]`} {
				operation := uuid.NewV7()
				var first int64
				for range 2 {
					seq, err := session.RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), turn.TurnID, operation, json.RawMessage(raw))
					if err != nil || seq < 1 || (first != 0 && seq != first) {
						t.Fatal(seq, err)
					}
					first = seq
				}
			}
			ask := uuid.NewV7()
			for range 2 {
				if err := session.RuntimeAsk(t.Context(), f.pool, *f.host(), f.execution(), turn.TurnID, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Continue?"}],"answer":{"type":"text"}}`)); err != nil {
					t.Fatal(err)
				}
			}
			var exact bool
			if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_threads)=$1 AND NOT EXISTS(SELECT 1 FROM slack_posts WHERE role='opening')`, map[bool]int{false: 0, true: 1}[routed]).Scan(&exact); err != nil || !exact {
				t.Fatal("runtime content changed conversation binding", exact, err)
			}
		})
	}
}

func TestSlackCompletionDoesNotCreateAnUnrequestedConversation(t *testing.T) {
	for _, response := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "explicit-empty"}[response], func(t *testing.T) {
			f := newFixture(t)
			storage := newSaveStorageFixture(t, f)
			slackRouteFixture(t, f)
			a := f.enqueue(t, "response")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			if response {
				if err := session.RuntimeRespond(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`[]`)); err != nil {
					t.Fatal(err)
				}
			}
			if err := session.RuntimeCloseProcessing(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
				t.Fatal(err)
			}
			if result, err := session.RuntimeFinalize(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`{"private":true}`), "joined"); err != nil || result != nil {
				t.Fatalf("prepare: %s %v", result, err)
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_threads`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("staging created root: %d %v", count, err)
			}
			var save uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `SELECT id FROM computer_saves WHERE turn_id=$1`, a.TurnID).Scan(&save); err != nil {
				t.Fatal(err)
			}
			root, _ := storage.cut(t, 8)
			if err := storage.publisher.Capture(t.Context(), storage.ref(save), root, "cut"); err != nil {
				t.Fatal(err)
			}
			if err := storage.publish(t, save, root); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := session.Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_threads`).Scan(&count); err != nil || count != want {
				t.Fatalf("completion roots: %d want %d: %v", count, want, err)
			}
		})
	}
}
