package slack

import (
	"bytes"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func (f statusFixture) nativeStop(t *testing.T, key string) uuid.UUID {
	t.Helper()
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	body := eventBody(t, key, map[string]any{"type": "agent_session_stopped", "channel": "C1", "thread_ts": "123.456", "event_ts": slackNow(), "user": "human", "streaming_message_ts": []string{}})
	for range 2 {
		if response := serveSignedEvent(handler, body); response.Code != http.StatusOK {
			t.Fatalf("Stop receipt: %d %s", response.Code, response.Body.String())
		}
	}
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE installation_id=$1 AND request_key=$2`, f.installation, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSlackNativeStopCoalescesAndResumesOnlyItsOwnHold(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	turn, ask := f.pendingQuestion(t)
	queued, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "user", ID: f.User, CredentialID: uuid.NewV7()}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "old-queue", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	first := f.nativeStop(t, "native-first")
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, first); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	second := f.nativeStop(t, "native-second")
	if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, second); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT a.status='accepted' AND b.status='accepted' AND a.control_id=b.control_id AND t.status='interrupted' AND q.status='cancelled' FROM slack_requests a JOIN slack_requests b ON b.id=$2 JOIN turns t ON t.id=$3 JOIN turn_asks q ON q.id=$4 WHERE a.id=$1`, first, second, turn, ask).Scan(&exact); err != nil || !exact {
		t.Fatalf("coalesced stop: %v %v", exact, err)
	}
	other, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.Caller{Kind: "user", ID: f.User, CredentialID: uuid.NewV7()}, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "interrupt", IdempotencyKey: "admin-hold"})
	if err != nil {
		t.Fatal(err)
	}
	incoming := f.reply(t, "resume", false)
	if err = admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), incoming); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT h.released_at IS NOT NULL AND other.released_at IS NULL AND incoming.status='accepted' AND t.status='queued' FROM slack_requests stopped JOIN session_controls c ON c.id=stopped.control_id JOIN session_holds h ON h.id=c.hold_id JOIN session_holds other ON other.id=$2 JOIN slack_requests incoming ON incoming.id=$3 JOIN turns t ON t.id=incoming.turn_id WHERE stopped.id=$1`, first, other.HoldID, incoming).Scan(&exact); err != nil || !exact {
		t.Fatalf("scoped resume: %v %v", exact, err)
	}
	var settled bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='interrupted' FROM turns WHERE id=$1`, queued.TurnID).Scan(&settled); err != nil || !settled {
		t.Fatalf("old queued work replayed: %v %v", settled, err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unexpected hold: %d %v", count, err)
	}
}

func TestSlackNativeStopCannotAdmitAgainstNewerExecutionOrChangedAuthority(t *testing.T) {
	for _, change := range []string{"turn", "relink", "role", "generation"} {
		t.Run(change, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			id := f.nativeStop(t, "delayed")
			switch change {
			case "turn":
				_, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "user", ID: f.User, CredentialID: uuid.NewV7()}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "newer", Input: json.RawMessage(`[]`)})
				if err != nil {
					t.Fatal(err)
				}
			case "relink":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_user_links SET linked_at=clock_timestamp() WHERE user_id=$1`, f.User)
			case "role":
				f.SetPermissions(t, "sessions.read")
			case "generation":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.Session)
			}
			if err := admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id); err != nil {
				t.Fatal(err)
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND NOT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$2) FROM slack_requests WHERE id=$1`, id, f.Environment).Scan(&exact); err != nil || !exact {
				t.Fatalf("stale/unauthorized stop: %v %v", exact, err)
			}
		})
	}
}

func TestSlackNativeStopAuthenticatesAndRejectsMalformedEventsBeforeSQL(t *testing.T) {
	h := EventHandler{AppID: "app", SigningSecret: []byte("fixture-secret")}
	valid := map[string]any{"type": "agent_session_stopped", "channel": "C1", "thread_ts": "123.456", "event_ts": slackNow(), "user": "human"}
	body := eventBody(t, "stop", valid)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body)))
	if response.Code != http.StatusUnauthorized {
		t.Fatal(response.Code)
	}
	for _, field := range []string{"user", "channel", "thread_ts", "event_ts"} {
		copy := map[string]any{}
		for k, v := range valid {
			copy[k] = v
		}
		copy[field] = ""
		if response := serveSignedEvent(h, eventBody(t, "invalid", copy)); response.Code != http.StatusBadRequest {
			t.Fatalf("%s reached database: %d", field, response.Code)
		}
	}
}

func TestSlackMentionAcknowledgesOnlyEarlierConversationRecoveryGap(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	stale := f.reply(t, "before-recovery", false)
	gap, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,kind,recovery_computer_id,recovery_lease_epoch,recovery_gap) VALUES($1,$2,$3,'local','Recovery acknowledgement required','recovery_gap',$4,1,jsonb_build_object('recoverySaveId',null,'restoredAt',null,'affectedTurnIds','[]'::jsonb))`, f.Environment, gap, f.Session, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','Independent operator hold')`, f.Environment, other, f.Session)
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), stale); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT released_at IS NULL FROM session_holds WHERE id=$1`, gap).Scan(&held); err != nil || !held {
		t.Fatalf("old mention acknowledged new loss: %v %v", held, err)
	}
	fresh := f.reply(t, "after-recovery", false)
	if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), fresh); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.released_at IS NOT NULL AND o.released_at IS NULL AND r.status='accepted' AND t.status='queued' FROM session_holds h JOIN session_holds o ON o.id=$2 JOIN slack_requests r ON r.id=$3 JOIN turns t ON t.id=r.turn_id WHERE h.id=$1`, gap, other, fresh).Scan(&exact); err != nil || !exact {
		t.Fatalf("recovery acknowledgement scope: %v %v", exact, err)
	}
}
