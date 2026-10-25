package slack

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func (f statusFixture) grantDirect(t *testing.T) {
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET granted_scopes=granted_scopes||ARRAY['im:history','im:read'] WHERE id=$1`, f.installation)
}
func (f statusFixture) directEvent(t *testing.T, key, ts, thread string) uuid.UUID {
	t.Helper()
	h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	e := map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "human", "ts": ts, "text": "do this &amp; that"}
	if thread != "" {
		e["thread_ts"] = thread
	}
	body := eventBody(t, key, e)
	for range 2 {
		if r := serveSignedEvent(h, body); r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key=$1`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func (f statusFixture) reconcileDirect(t *testing.T, id uuid.UUID) {
	t.Helper()
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), directClient(t, "human", nil), id); err != nil {
		t.Fatal(err)
	}
}
func (f statusFixture) receiptStatus(t *testing.T, id uuid.UUID, want string) {
	t.Helper()
	var got string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_requests WHERE id=$1`, id).Scan(&got); err != nil || got != want {
		t.Fatalf("receipt %s want %s: %v", got, want, err)
	}
}

func TestSlackDirectRootReplyRaceAndSeparateSessions(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.grantDirect(t)
	f.link(t)
	root := slackNow()
	reply := f.directEvent(t, "reply", slackNow(), root)
	f.reconcileDirect(t, reply)
	f.receiptStatus(t, reply, "received")
	first := f.directEvent(t, "root", root, root)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), directClient(t, "human", nil), first); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.reconcileDirect(t, reply)
	second := f.directEvent(t, "separate", slackNow(), "")
	f.reconcileDirect(t, second)
	for _, id := range []uuid.UUID{first, reply, second} {
		f.receiptStatus(t, id, "accepted")
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=3 AND count(DISTINCT session_id)=2 AND count(*) FILTER(WHERE operation='start')=2 FROM slack_requests WHERE status='accepted'`).Scan(&exact); err != nil || !exact {
		t.Fatal("wrong session grouping", exact, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(recipient_user_id='human' AND recipient_team_id='team') FROM slack_threads WHERE slack_channel_id='D1'`).Scan(&exact); err != nil || !exact {
		t.Fatal("missing durable peer", exact, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(convert_from(input,'UTF8')::jsonb='[{"type":"text","text":"do this & that"}]'::jsonb) FROM turns`).Scan(&exact); err != nil || !exact {
		t.Fatal("input changed", exact, err)
	}
}

func TestSlackDirectRejectedRootNeverReplaysAfterLink(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.grantDirect(t)
	root := slackNow()
	first := f.directEvent(t, "unlinked", root, "")
	f.reconcileDirect(t, first)
	f.receiptStatus(t, first, "rejected")
	calls := 0
	c := directClient(t, "human", func(r *http.Request) string {
		calls++
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/api/chat.postMessage" || p["channel"] != "D1" || p["user"] != nil || !strings.Contains(p["text"].(string), "People with session viewing or management access") {
			t.Fatal(r.URL, p)
		}
		return `{"ok":true,"ts":"123.999"}`
	})
	for range 2 {
		if _, err := ReconcileRejectedFeedback(t.Context(), f.Pool, c, testProjectionConfig(), 4); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("duplicate feedback", calls)
	}
	f.link(t)
	f.reconcileDirect(t, first)
	reply := f.directEvent(t, "old-thread", slackNow(), root)
	f.reconcileDirect(t, reply)
	f.receiptStatus(t, reply, "received")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, reply)
	f.reconcileDirect(t, reply)
	f.receiptStatus(t, reply, "rejected")
	fresh := f.directEvent(t, "retry", slackNow(), "")
	f.reconcileDirect(t, fresh)
	f.receiptStatus(t, fresh, "accepted")
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns`).Scan(&n); err != nil || n != 1 {
		t.Fatal("replayed rejected input", n, err)
	}
}

func TestSlackDirectRequiresGrantsAndChannelKeepsWorking(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	old := f.directEvent(t, "missing-grants", slackNow(), "")
	f.reconcileDirect(t, old)
	f.receiptStatus(t, old, "rejected")
	channel := f.reply(t, "channel", false)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), channel); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus(t, channel, "accepted")
	post := f.post(t, 1, "content", "lifecycle", nil)
	if claim, err := ClaimPost(t.Context(), f.Pool, post); err != nil || claim == nil {
		t.Fatal("old-grant channel delivery blocked", err)
	}
	f.grantDirect(t)
	fresh := f.directEvent(t, "granted", slackNow(), "")
	f.reconcileDirect(t, fresh)
	f.receiptStatus(t, fresh, "accepted")
}

func TestSlackDirectReplyRejectsAuthorityChanges(t *testing.T) {
	for _, kind := range []string{"peer", "missing-peer", "revoked", "reauthorized", "closed", "deleted", "grants"} {
		t.Run(kind, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.grantDirect(t)
			f.link(t)
			root := slackNow()
			first := f.directEvent(t, "root", root, "")
			f.reconcileDirect(t, first)
			f.receiptStatus(t, first, "accepted")
			reply := f.directEvent(t, "reply", slackNow(), root)
			switch kind {
			case "peer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET recipient_user_id='other' WHERE slack_channel_id='D1'`)
			case "missing-peer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET recipient_user_id=NULL,recipient_team_id=NULL WHERE slack_channel_id='D1'`)
			case "revoked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE user_id=$1`, f.User)
			case "reauthorized":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=clock_timestamp() WHERE id=$1`, f.installation)
			case "deleted":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET deleted_at=clock_timestamp() WHERE slack_channel_id='D1'`)
			case "closed":
				var sid uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT session_id FROM slack_requests WHERE id=$1`, first).Scan(&sid); err != nil {
					t.Fatal(err)
				}
				if _, err := session.ControlSession(t.Context(), f.Pool, session.Caller{Kind: "user", ID: f.User}, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: sid, Kind: "cancel", IdempotencyKey: "close"}); err != nil {
					t.Fatal(err)
				}
			case "grants":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET granted_scopes=array_remove(granted_scopes,'im:read') WHERE id=$1`, f.installation)
			}
			f.reconcileDirect(t, reply)
			f.receiptStatus(t, reply, "rejected")
		})
	}
}

func TestSlackDirectAppHomeOnlySetsVerifiedPrompts(t *testing.T) {
	for _, kind := range []string{"valid", "wrong-peer", "missing-grants", "home", "bot", "api-rejected"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			if kind != "missing-grants" {
				f.grantDirect(t)
			}
			peer := "human"
			if kind == "wrong-peer" {
				peer = "other"
			}
			calls := 0
			c := directClient(t, peer, func(r *http.Request) string {
				calls++
				var p map[string]any
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/api/assistant.threads.setSuggestedPrompts" || p["channel_id"] != "D1" || p["thread_ts"] != nil || !strings.Contains(p["title"].(string), "session viewing or management access") {
					t.Fatal(r.URL, p)
				}
				if kind == "api-rejected" {
					return `{"ok":false,"error":"missing_scope"}`
				}
				return `{"ok":true}`
			})
			h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), Client: c}
			e := map[string]any{"type": "app_home_opened", "tab": "messages", "channel": "D1", "user": "human"}
			if kind == "home" {
				e["tab"] = "home"
			}
			if kind == "bot" {
				e["user"] = "bot"
			}
			for range 2 {
				r := serveSignedEvent(h, eventBody(t, "open", e))
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
			}
			want := 0
			if kind == "valid" || kind == "api-rejected" {
				want = 2
			}
			if calls != want {
				t.Fatal(calls, want)
			}
			var n int
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_requests)+(SELECT count(*) FROM turns)`).Scan(&n); err != nil || n != 0 {
				t.Fatal("tab open admitted work", n, err)
			}
		})
	}
}

func directBoundFixture(t *testing.T) statusFixture {
	f := newStatusFixture(t)
	f.grantDirect(t)
	f.link(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $3,environment_id,connection_id,installation_id,org_id,team_id,'D1' FROM slack_channel_bindings WHERE id=$1;
 UPDATE slack_threads SET slack_channel_binding_id=$3,slack_channel_id='D1',recipient_user_id='human',recipient_team_id='team' WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.channel, f.thread, uuid.NewV7())
	return f
}

func TestSlackDirectAnswersAndNativeStopVerifyPeer(t *testing.T) {
	for _, kind := range []string{"answer", "stop", "wrong-peer-answer", "wrong-peer-stop"} {
		t.Run(kind, func(t *testing.T) {
			f := directBoundFixture(t)
			var id uuid.UUID
			if strings.HasSuffix(kind, "answer") {
				turn, ask := f.pendingQuestion(t)
				c := f.control("answer")
				c.Target.Turn, c.Target.Ask = turn, ask
				id = f.controlReceipt(t, c, json.RawMessage(`"yes"`))
			} else {
				h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
				r := serveSignedEvent(h, eventBody(t, "stop", map[string]any{"type": "agent_session_stopped", "channel": "D1", "thread_ts": "123.456", "event_ts": slackNow(), "user": "human"}))
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key='stop'`).Scan(&id); err != nil {
					t.Fatal(err)
				}
			}
			peer := "human"
			want := "accepted"
			if strings.HasPrefix(kind, "wrong-peer") {
				peer = "other"
				want = "rejected"
			}
			if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), directClient(t, peer, nil), id); err != nil {
				t.Fatal(err)
			}
			f.receiptStatus(t, id, want)
		})
	}
}

func TestSlackDirectDeliveryVerifiesRetainedPeer(t *testing.T) {
	for _, kind := range []string{"post", "stream", "status", "wrong-peer-post", "wrong-peer-status"} {
		t.Run(kind, func(t *testing.T) {
			f := directBoundFixture(t)
			peer := "human"
			if strings.HasPrefix(kind, "wrong-peer") {
				peer = "other"
			}
			calls := 0
			c := directClient(t, peer, func(r *http.Request) string {
				calls++
				var p map[string]any
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Fatal(err)
				}
				if p["thread_ts"] != "123.456" || (p["channel"] != "D1" && p["channel_id"] != "D1") {
					t.Fatal("wrong destination", p)
				}
				if strings.HasSuffix(kind, "status") {
					return `{"ok":true,"agent_status":"active"}`
				}
				return `{"ok":true,"ts":"123.999","channel":"D1"}`
			})
			var err error
			if strings.HasSuffix(kind, "status") {
				_, err = ReconcileStatus(t.Context(), f.Pool, c, f.thread)
			} else {
				var post uuid.UUID
				if kind == "stream" {
					post = f.streamPost(t, 1, "response")
				} else {
					post = f.post(t, 1, "content", "lifecycle", nil)
				}
				_, err = ReconcilePost(t.Context(), f.Pool, c, post)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if peer == "other" {
				want = 0
			}
			if calls != want {
				t.Fatal("wrong peer delivery", calls, want)
			}
		})
	}
}

func TestSlackDirectControlsExpireWithoutRemoteAccess(t *testing.T) {
	for _, kind := range []string{"answer-expired", "stop-expired", "answer-revoked", "stop-revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := directBoundFixture(t)
			var id uuid.UUID
			if strings.HasPrefix(kind, "answer") {
				c := f.control("answer")
				c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
				id = f.controlReceipt(t, c, json.RawMessage(`"yes"`))
			} else {
				h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
				r := serveSignedEvent(h, eventBody(t, "stop", map[string]any{"type": "agent_session_stopped", "channel": "D1", "thread_ts": "123.456", "event_ts": slackNow(), "user": "human"}))
				if r.Code != 200 {
					t.Fatal(r.Code)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_requests WHERE request_key='stop'`).Scan(&id); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(kind, "expired") {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			}
			// No client proves that expiry/known route loss precedes token or API access.
			if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), nil, id); err != nil {
				t.Fatal(err)
			}
			f.receiptStatus(t, id, "rejected")
		})
	}
}

func TestSlackDirectModalReadCannotCrossPeer(t *testing.T) {
	for _, actor := range []string{"human", "other"} {
		t.Run(actor, func(t *testing.T) {
			f := directBoundFixture(t)
			if actor == "other" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_user_links SET slack_user_id='other' WHERE user_id=$1`, f.User)
			}
			c := f.control("open_answer")
			c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				view, _, err := readFormQuestion(t.Context(), tx, "app", "team", actor, c, time.Now())
				if actor == "human" && err == nil && view.ID != c.Target.Ask {
					t.Fatal("wrong question")
				}
				if actor != "human" && len(view.Question) > 0 {
					t.Fatal("question disclosed to another peer")
				}
				return err
			})
			if (err == nil) != (actor == "human") {
				t.Fatal(actor, err)
			}
		})
	}
}

func TestSlackDirectUncertainDeliveryDoesNotReplayAndAcceptsExactEvidence(t *testing.T) {
	f := directBoundFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if claim.PeerID != "human" {
		t.Fatal("missing peer on durable delivery")
	}
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, post)
	m := observedClaim(t, claim)
	m.Channel = "D2"
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || ok {
		t.Fatal("cross-DM evidence accepted", ok, err)
	}
	m.Channel = "D1"
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
		t.Fatal("exact DM evidence not applied", ok, err)
	}
	f.due(t)
	f.noPostClaim(t, post)
}

func TestSlackDirectIngressIgnoresNonHumanAndWrongConversationEvents(t *testing.T) {
	f := newStatusFixture(t)
	f.grantDirect(t)
	h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	for _, kind := range []string{"bot", "own-bot", "edit", "subtype", "mpim", "missing-type", "mention-event", "wrong-team", "wrong-app", "external-user", "external-source"} {
		e := map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "human", "ts": slackNow(), "text": "private input"}
		switch kind {
		case "bot":
			e["bot_id"] = "other"
		case "own-bot":
			e["user"] = "bot"
		case "edit":
			e["edited"] = map[string]string{"ts": slackNow()}
		case "subtype":
			e["subtype"] = "thread_broadcast"
		case "mpim":
			e["channel_type"] = "mpim"
		case "missing-type":
			delete(e, "channel_type")
		case "mention-event":
			e["type"] = "app_mention"
		case "external-user":
			e["user_team"] = "external"
		case "external-source":
			e["source_team"] = "external"
		}
		body := eventBody(t, kind, e)
		if kind == "wrong-team" {
			body = []byte(strings.Replace(string(body), `"team_id":"team"`, `"team_id":"other"`, 1))
		}
		if kind == "wrong-app" {
			body = []byte(strings.Replace(string(body), `"api_app_id":"app"`, `"api_app_id":"other"`, 1))
		}
		r := serveSignedEvent(h, body)
		want := 200
		if kind == "wrong-app" {
			want = 401
		}
		if r.Code != want {
			t.Fatal(kind, r.Code)
		}
	}
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatal("unrelated input retained", n, err)
	}
}

func TestDirectDeliveryRechecksRecipientUseAndIdentity(t *testing.T) {
	for _, revoke := range []string{"permission", "link", "membership"} {
		t.Run(revoke, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.grantDirect(t)
			f.link(t)
			id := f.directEvent(t, "direct-revoke", slackNow(), "")
			f.reconcileDirect(t, id)
			f.receiptStatus(t, id, "accepted")
			var thread uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT thread_id FROM slack_requests WHERE id=$1`, id).Scan(&thread); err != nil {
				t.Fatal(err)
			}
			check := func(want bool) {
				t.Helper()
				err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
					allowed, err := directRecipientAuthorized(t.Context(), tx, thread)
					if err == nil && allowed != want {
						t.Fatalf("recipient allowed=%v want=%v", allowed, want)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			check(true)
			switch revoke {
			case "permission":
				f.SetPermissions(t, "sessions.read")
			case "link":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM slack_user_links WHERE user_id=$1`, f.User)
			case "membership":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET removed_at=clock_timestamp() WHERE user_id=$1`, f.User)
			}
			check(false)
		})
	}
}
