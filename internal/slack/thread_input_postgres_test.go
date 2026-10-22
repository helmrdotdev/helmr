package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func (f statusFixture) threadMessage(t *testing.T, key, text string, at time.Time) uuid.UUID {
	t.Helper()
	raw, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: fmt.Sprintf("%d.%06d", at.Unix(), at.Nanosecond()/1000), Thread: "123.456", Text: text})
	r, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: at, ExpiresAt: at.Add(time.Minute), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}
func (f statusFixture) admitThreadMessage(t *testing.T, id uuid.UUID) {
	t.Helper()
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
}
func (f statusFixture) assertThreadInput(t *testing.T, muted bool, turns int) {
	t.Helper()
	var got bool
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT input_muted,(SELECT count(*) FROM turns WHERE session_id=$2) FROM slack_threads WHERE id=$1`, f.thread, f.Session).Scan(&got, &n); err != nil || got != muted || n != turns {
		t.Fatalf("muted=%v turns=%d want %v/%d: %v", got, n, muted, turns, err)
	}
}

func TestSlackThreadInputMuteIsPersistentAndNeverReplays(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	start := time.Now()
	first := f.threadMessage(t, "ordinary", "continue without mention", start)
	f.admitThreadMessage(t, first)
	mute := f.threadMessage(t, "mute", "!mute", start.Add(time.Second))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), mute); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.assertThreadInput(t, true, 1)
	ignored := f.threadMessage(t, "muted", "human discussion", start.Add(2*time.Second))
	f.admitThreadMessage(t, ignored)
	f.receiptStatus(t, ignored, "rejected")
	override := f.threadMessage(t, "override", "<@bot> do this one thing", start.Add(3*time.Second))
	f.admitThreadMessage(t, override)
	f.assertThreadInput(t, true, 2)
	unmute := f.threadMessage(t, "unmute", "!unmute", start.Add(4*time.Second))
	f.admitThreadMessage(t, unmute)
	f.assertThreadInput(t, false, 2)
	delayed := f.threadMessage(t, "delayed", "late delivery from muted period", start.Add(2500*time.Millisecond))
	f.admitThreadMessage(t, delayed)
	f.receiptStatus(t, delayed, "rejected")
	staleMute := f.threadMessage(t, "stale-mute", "!mute", start.Add(3500*time.Millisecond))
	f.admitThreadMessage(t, staleMute)
	f.receiptStatus(t, staleMute, "rejected")
	f.assertThreadInput(t, false, 2)
	after := f.threadMessage(t, "after", "new work", start.Add(5*time.Second))
	f.admitThreadMessage(t, after)
	f.assertThreadInput(t, false, 3)
	// Each successful command has one private confirmation, not an Agent turn.
	var seen []string
	for {
		feedback, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if feedback == nil {
			continue
		}
		var body map[string]any
		if err = json.Unmarshal(feedback.payload, &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body["text"].(string), "Thread input") {
			if body["user"] != "human" || body["thread_ts"] != "123.456" {
				t.Fatal(body)
			}
			seen = append(seen, body["text"].(string))
		}
	}
	if len(seen) != 2 {
		t.Fatalf("command confirmations: %v", seen)
	}
	f.admitThreadMessage(t, mute)
	f.assertThreadInput(t, false, 3)
}

func TestSlackThreadInputControlsDoNotReleaseStopOrChangeCoreWork(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	stop := f.nativeStop(t, "stop")
	if err := admitNativeStop(t.Context(), f.Pool, stop); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"!mute", "!unmute", "!aside <@bot> discussion"} {
		id := f.threadMessage(t, text, text, time.Now())
		f.admitThreadMessage(t, id)
	}
	var held bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1 AND released_at IS NULL) AND NOT EXISTS(SELECT 1 FROM turns WHERE session_id=$1)`, f.Session).Scan(&held); err != nil || !held {
		t.Fatalf("commands released hold or created work: %v %v", held, err)
	}
	id := f.threadMessage(t, "resume", "please continue", time.Now())
	f.admitThreadMessage(t, id)
	if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1 AND released_at IS NULL)`, f.Session).Scan(&held); err != nil || held {
		t.Fatalf("normal continuation failed to resume native Stop: %v %v", held, err)
	}
}

func TestSlackThreadInputControlAuthorityAndTerminalState(t *testing.T) {
	for _, scenario := range []string{"unlinked", "viewer", "closed", "unbound", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStatusFixture(t)
			if scenario != "unlinked" {
				f.link(t)
			}
			id := f.threadMessage(t, "mute", "!mute", time.Now())
			switch scenario {
			case "viewer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='viewer' WHERE user_id=$1`, f.User)
			case "closed":
				if _, err := session.ControlSession(t.Context(), f.Pool, session.Caller{Kind: "user", ID: f.User}, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
					t.Fatal(err)
				}
			case "unbound":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts='234.567' WHERE id=$1`, f.thread)
			case "expired":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
			}
			f.admitThreadMessage(t, id)
			f.receiptStatus(t, id, "rejected")
			f.assertThreadInput(t, false, 0)
		})
	}
}

func TestSlackThreadInputEventBoundaryAndMuteExclusion(t *testing.T) {
	f := newStatusFixture(t)
	h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	send := func(key, text, thread string, want int) {
		t.Helper()
		body := eventBody(t, key, map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": thread, "text": text})
		if r := serveSignedEvent(h, body); r.Code != 200 {
			t.Fatal(r.Code)
		}
		var n int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests WHERE request_key=$1`, key).Scan(&n); err != nil || n != want {
			t.Fatalf("%s receipts=%d: %v", key, n, err)
		}
	}
	send("before-visible", "continue", "123.456", 0)
	first := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	send("bound", "continue", "123.456", 1)
	f.due(t)
	refresh := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, refresh, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	send("bound-after-uncertainty", "continue", "123.456", 1)
	// Equal values with an uncertain outcome still need an immediate retry when due.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET next_attempt_at=clock_timestamp() WHERE id=$1;
 UPDATE slack_delivery_limits SET next_status_at=clock_timestamp() WHERE installation_id=$2`, pgx.QueryExecModeSimpleProtocol, f.thread, f.installation)
	retry := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, retry, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	send("unbound", "continue", "999.123", 0)
	send("root", "continue", "", 0)
	send("aside", "!aside <@bot> human discussion", "123.456", 0)
	send("literal", "`!aside` is code", "123.456", 1)
	send("other-human", "<@colleague> discuss this", "123.456", 1)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET input_muted=true,input_changed_at=clock_timestamp() WHERE id=$1`, f.thread)
	send("muted", "human discussion", "123.456", 0)
	send("mention", "<@bot> just this message", "123.456", 1)
	send("unmute", "!unmute", "123.456", 1)
}

func TestSlackThreadInputMuteReceiptFailureRollsBackState(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.threadMessage(t, "mute", "!mute", time.Now())
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests ADD CONSTRAINT no_acceptance CHECK(status<>'accepted')`)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err == nil {
		t.Fatal("expected receipt constraint failure")
	}
	f.assertThreadInput(t, false, 0)
	f.receiptStatus(t, id, "received")
}

func TestSlackDirectThreadInputCommandsRespectPeerAndDoNotStartSessions(t *testing.T) {
	for _, scenario := range []string{"mute", "wrong-peer", "unbound", "aside"} {
		t.Run(scenario, func(t *testing.T) {
			f := directBoundFixture(t)
			if scenario == "wrong-peer" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET recipient_user_id='other' WHERE id=$1`, f.thread)
			}
			root := "123.456"
			text := "!mute"
			if scenario == "unbound" {
				root = ""
			}
			if scenario == "aside" {
				text = "!aside <@bot> discussion"
			}
			h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
			e := map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "human", "ts": slackNow(), "thread_ts": root, "text": text}
			if r := serveSignedEvent(h, eventBody(t, "command", e)); r.Code != 200 {
				t.Fatal(r.Code)
			}
			var n int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if scenario == "aside" {
				if n != 0 {
					t.Fatal("aside persisted input")
				}
				return
			}
			var id uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key='command'`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			f.reconcileDirect(t, id)
			want := "rejected"
			if scenario == "mute" {
				want = "accepted"
			}
			f.receiptStatus(t, id, want)
			f.assertThreadInput(t, scenario == "mute", 0)
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("command started Session: %d %v", n, err)
			}
			if scenario == "mute" {
				hint, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
				if err != nil || !found || hint == nil {
					t.Fatal("missing DM command confirmation", err)
				}
				var body map[string]any
				if err = json.Unmarshal(hint.payload, &body); err != nil || body["channel"] != "D1" || body["thread_ts"] != "123.456" || body["user"] != nil {
					t.Fatalf("DM confirmation: %s %v", hint.payload, err)
				}
			}
		})
	}
}

func TestSlackThreadInputRedundantUnmuteDoesNotDiscardEarlierWork(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	start := time.Now()
	unmute := f.threadMessage(t, "noop-unmute", "!unmute", start.Add(time.Second))
	f.admitThreadMessage(t, unmute)
	earlier := f.threadMessage(t, "earlier", "continue", start)
	f.admitThreadMessage(t, earlier)
	f.receiptStatus(t, earlier, "accepted")
	f.assertThreadInput(t, false, 1)
	stale := f.threadMessage(t, "stale-command", "!mute", start)
	f.admitThreadMessage(t, stale)
	f.receiptStatus(t, stale, "rejected")
	f.assertThreadInput(t, false, 1)
}

func TestSlackDirectThreadInputCommandWaitsForItsRoot(t *testing.T) {
	f := directBoundFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts='234.567' WHERE id=$1`, f.thread)
	raw, _ := json.Marshal(messageGesture{Channel: "D1", Timestamp: slackNow(), Thread: "123.456", Text: "!mute"})
	r, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "command-before-root", Actor: "human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	f.reconcileDirect(t, r.ID)
	f.receiptStatus(t, r.ID, "received")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts='123.456' WHERE id=$1`, f.thread)
	f.reconcileDirect(t, r.ID)
	f.receiptStatus(t, r.ID, "accepted")
	f.assertThreadInput(t, true, 0)
}

func TestSlackThreadInputUnmentionedRejectionKeepsPrivateGuidance(t *testing.T) {
	for _, role := range []string{"unlinked", "viewer"} {
		t.Run(role, func(t *testing.T) {
			f := newStatusFixture(t)
			if role == "viewer" {
				f.link(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='viewer' WHERE user_id=$1`, f.User)
			}
			id := f.threadMessage(t, "ordinary", "please continue", time.Now())
			f.admitThreadMessage(t, id)
			f.receiptStatus(t, id, "rejected")
			feedback, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
			if err != nil || !found || feedback == nil {
				t.Fatalf("missing private guidance: %v %v", found, err)
			}
			var body map[string]any
			if err = json.Unmarshal(feedback.payload, &body); err != nil || body["user"] != "human" {
				t.Fatalf("guidance: %s %v", feedback.payload, err)
			}
			f.assertThreadInput(t, false, 0)
		})
	}
}
