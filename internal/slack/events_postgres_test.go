package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func slackNow() string {
	now := time.Now()
	return fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
}

func eventBody(t *testing.T, id string, event map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"type": "event_callback", "api_app_id": "app", "team_id": "team", "event_id": id, "event": event})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func serveSignedEvent(handler EventHandler, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
	request.Header = signedHeaders(handler.SigningSecret, body, time.Now())
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSlackEventHandlerAuthenticatesBeforeDurableReceiptAndNeverAdmitsWork(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	body := eventBody(t, "event-1", map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> work"})
	unsigned := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unsigned)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned callback accepted: %d", response.Code)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unsigned request persisted: %d %v", count, err)
	}
	for range 2 {
		response = serveSignedEvent(handler, body)
		if response.Code != http.StatusOK {
			t.Fatalf("signed event not acknowledged: %d", response.Code)
		}
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests WHERE request_key='event-1' AND status='received'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ACK did not follow one receipt: %d %v", count, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("HTTP receipt admitted work: %d %v", count, err)
	}
}

func TestSlackEventHandlerIgnoresBotsEditsAndUnboundChatter(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	for _, kind := range []string{"bot-id", "app-id", "bot-profile", "own-bot", "no-human", "edited", "deleted", "unbound", "top-chatter"} {
		event := map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": "123.456", "text": "hello"}
		switch kind {
		case "bot-id":
			event["bot_id"] = "another-bot"
		case "app-id":
			event["app_id"] = "other-app"
		case "bot-profile":
			event["bot_profile"] = map[string]any{"id": "another-bot"}
		case "own-bot":
			event["user"] = "bot"
		case "no-human":
			delete(event, "user")
		case "edited":
			event["edited"] = map[string]any{"ts": slackNow()}
		case "deleted":
			event["subtype"] = "message_deleted"
		case "unbound":
			event["thread_ts"] = "987.123"
		case "top-chatter":
			delete(event, "thread_ts")
		}
		if response := serveSignedEvent(handler, eventBody(t, kind, event)); response.Code != http.StatusOK {
			t.Fatalf("ignored %s response %d", kind, response.Code)
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unrelated content retained: %d %v", count, err)
	}
	event := map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": "123.456", "text": "follow-up"}
	if response := serveSignedEvent(handler, eventBody(t, "follow-up", event)); response.Code != http.StatusOK {
		t.Fatalf("bound human follow-up response %d", response.Code)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bound follow-up missing: %d %v", count, err)
	}
}

func TestSlackEventHandlerChallengeAndStaleSource(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	challenge := serveSignedEvent(handler, []byte(`{"type":"url_verification","challenge":"verify-me"}`))
	if challenge.Code != http.StatusOK || challenge.Body.String() != "{\"challenge\":\"verify-me\"}\n" {
		t.Fatalf("challenge response: %d %s", challenge.Code, challenge.Body.String())
	}
	event := map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": "123.456", "text": "<@bot> old"}
	if response := serveSignedEvent(handler, eventBody(t, "old-event", event)); response.Code != http.StatusOK {
		t.Fatalf("stale source not receipted: %d", response.Code)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='stale_gesture' FROM slack_requests WHERE request_key='old-event'`).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("fresh transport revived old gesture: %v %v", rejected, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
	event["ts"] = slackNow()
	if response := serveSignedEvent(handler, eventBody(t, "lost-auth", event)); response.Code != http.StatusOK {
		t.Fatalf("known-loss source not receipted: %d", response.Code)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='installation_unavailable' FROM slack_requests WHERE request_key='lost-auth'`).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("known auth loss parked input: %v %v", rejected, err)
	}
}

func TestSlackEventHandlerIgnoresBroadcastOfReceivedReply(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	event := map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": "123.456", "text": "original reply"}
	if response := serveSignedEvent(handler, eventBody(t, "original", event)); response.Code != http.StatusOK {
		t.Fatalf("reply response: %d", response.Code)
	}
	event["subtype"] = "thread_broadcast"
	if response := serveSignedEvent(handler, eventBody(t, "broadcast", event)); response.Code != http.StatusOK {
		t.Fatalf("broadcast response: %d", response.Code)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("broadcast became another input: %d %v", count, err)
	}
}

func TestSlackEventHandlerDeduplicatesMessageNotifications(t *testing.T) {
	for _, order := range []string{"mention-first", "message-first", "concurrent", "retired"} {
		t.Run(order, func(t *testing.T) {
			f := newStatusFixture(t)
			handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
			event := map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": "123.456", "text": "<@bot> work", "files": []any{map[string]any{"id": "F1"}}}
			message := eventBody(t, "message-id", event)
			event["type"] = "app_mention"
			mention := eventBody(t, "mention-id", event)
			first, second := message, mention
			if order == "mention-first" {
				first, second = mention, message
			}
			send := func(body []byte) {
				if response := serveSignedEvent(handler, body); response.Code != http.StatusOK {
					t.Errorf("event response: %d", response.Code)
				}
			}
			if order == "concurrent" {
				var wg sync.WaitGroup
				for _, body := range [][]byte{first, second, first, second} {
					wg.Go(func() { send(body) })
				}
				wg.Wait()
			} else {
				send(first)
				if order == "retired" {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error='gesture_expired',payload=NULL,payload_expired_at=clock_timestamp()`)
				}
				send(second)
			}
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("notification duplicated gesture: %d %v", count, err)
			}
			event["ts"] = slackNow()
			send(eventBody(t, "new-message", event))
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("new source message lost: %d %v", count, err)
			}
			event["text"] = "changed content"
			if response := serveSignedEvent(handler, eventBody(t, "new-message", event)); response.Code != http.StatusConflict {
				t.Fatalf("existing event key bypassed conflict: %d", response.Code)
			}
		})
	}
}

func TestSlackEventHandlerRetainsLiteralMentionInCode(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	event := map[string]any{"type": "message", "user": "human", "channel": "C1", "ts": slackNow(), "thread_ts": "123.456", "text": "explain `<@bot>` syntax"}
	if response := serveSignedEvent(handler, eventBody(t, "literal-mention", event)); response.Code != http.StatusOK {
		t.Fatalf("literal response: %d", response.Code)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests WHERE request_key='literal-mention'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("literal mention discarded reply: %d %v", count, err)
	}
}

func TestSlackEventHandlerDoesNotDeduplicateDifferentTargetsOrFiles(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	event := map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> review", "files": []any{map[string]any{"id": "F1"}}}
	for index := range 3 {
		if index == 1 {
			event["files"] = []any{map[string]any{"id": "F2"}}
		}
		if index == 2 {
			event["channel"] = "C2"
		}
		if response := serveSignedEvent(handler, eventBody(t, fmt.Sprintf("distinct-%d", index), event)); response.Code != http.StatusOK {
			t.Fatalf("response: %d", response.Code)
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("different file or target discarded: %d %v", count, err)
	}
}

func TestSlackEventReconcilesOnlyExactAuthenticatedProjection(t *testing.T) {
	for _, kind := range []string{"posted", "changed", "human", "other-app", "other-channel", "other-team", "changed-content", "disconnected-generation", "unsigned"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			if kind == "changed" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET message_ts='123.789' WHERE id=$1`, post)
			}
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
				t.Fatal(err)
			}
			message := observedClaim(t, claim)
			switch kind {
			case "human":
				message.User = "human"
			case "other-app":
				message.AppID = "another-app"
			case "other-channel":
				message.Channel = "another-channel"
			case "changed-content":
				message.Text = "changed"
			case "disconnected-generation":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET disconnected_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, f.installation)
			}
			raw, _ := json.Marshal(message)
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			event["type"] = "message"
			event["subtype"] = "bot_message"
			if kind == "changed" {
				event = map[string]any{"type": "message", "subtype": "message_changed", "hidden": true, "channel": "C1", "message": message, "ts": slackNow()}
			}
			body := eventBody(t, "projection", event)
			if kind == "other-team" {
				body = bytes.Replace(body, []byte(`"team_id":"team"`), []byte(`"team_id":"another-team"`), 1)
			}
			handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
			if kind == "unsigned" {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body)))
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("unsigned event: %d", response.Code)
				}
			} else {
				for range 2 {
					if response := serveSignedEvent(handler, body); response.Code != http.StatusOK {
						t.Fatalf("event response: %d", response.Code)
					}
				}
			}
			var posted bool
			var requests int
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='posted' FROM slack_posts WHERE id=$1),(SELECT count(*) FROM slack_requests)`, post).Scan(&posted, &requests); err != nil {
				t.Fatal(err)
			}
			expected := kind == "posted" || kind == "changed" || kind == "disconnected-generation"
			if posted != expected || requests != 0 {
				t.Fatalf("wrong reconciliation/work admission: posted=%v requests=%d", posted, requests)
			}
		})
	}
}

func TestSlackSignedStreamEventConfirmsUnknownPublication(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "bot_message", true: "message_changed"}[changed], func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.streamPost(t, 1, "streamed")
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			message := observedStream(t, f, post, "streamed", "completed")
			raw, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err = json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			event["type"] = "message"
			event["subtype"] = "bot_message"
			if changed {
				event = map[string]any{"type": "message", "subtype": "message_changed", "channel": message.Channel, "message": event}
			}
			handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
			response := serveSignedEvent(handler, eventBody(t, "stream-observed", event))
			if response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body.String())
			}
			var settled bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND stream_state='stopped' AND message_ts=$2 FROM slack_posts WHERE id=$1`, post, message.Timestamp).Scan(&settled); err != nil || !settled {
				t.Fatal("signed event not applied", err)
			}
		})
	}
}

func TestSlackEventReceiptDoesNotWaitForCoreAdmission(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "blocked-admission", false)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err = blocker.Exec(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.Environment); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id) }()
	defer func() {
		_ = blocker.Rollback(t.Context())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	observed := false
	deadline := time.Now().Add(5 * time.Second)
	for !observed && time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if !observed {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !observed {
		t.Fatal("admission did not hold installation while waiting on core")
	}
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	event := map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> independent action"}
	if response := serveSignedEvent(handler, eventBody(t, "independent-receipt", event)); response.Code != http.StatusOK {
		t.Fatal("receipt blocked on core admission", response.Code)
	}
	var received bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='received' FROM slack_requests WHERE request_key='independent-receipt'`).Scan(&received); err != nil || !received {
		t.Fatal("receipt not durable before acknowledgment", received, err)
	}
}
