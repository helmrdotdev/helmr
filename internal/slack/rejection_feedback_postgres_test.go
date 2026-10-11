package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackRejectedFeedbackIsPrivateOnceAndDoesNotAdmitWork(t *testing.T) {
	f := newStatusFixture(t)
	for i := range 8 {
		id := f.reply(t, fmt.Sprintf("unlinked-%d", i), false)
		if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	client := NewWebClient(credentialsFunc(func(_ context.Context, id uuid.UUID, revision int64) (string, error) {
		if id != f.installation || revision != 1 {
			t.Error("wrong credential owner")
		}
		return "fixture", nil
	}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		var body struct {
			Channel string            `json:"channel"`
			User    string            `json:"user"`
			Text    string            `json:"text"`
			Blocks  []json.RawMessage `json:"blocks"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/api/chat.postEphemeral" || len(body.Blocks) != 2 || body.Channel != "C1" || body.User != "human" || !strings.Contains(body.Text, "Link your Slack account") || strings.Contains(body.Text, "follow-up") {
			t.Errorf("private destination/content: %+v", body)
		}
		// A failed HTTP response never creates another attempt.
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			if _, err := ReconcileRejectedFeedback(t.Context(), f.Pool, client, testProjectionConfig(), 8); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if calls.Load() != 8 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if n, err := ReconcileRejectedFeedback(t.Context(), f.Pool, client, testProjectionConfig(), 8); err != nil || n != 0 {
		t.Fatalf("repeat: %d %v", n, err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*)=8 FROM slack_requests WHERE status='rejected' AND payload_expired_at IS NOT NULL AND payload IS NULL) AND (SELECT count(*)=0 FROM turns) AND (SELECT count(*)=0 FROM slack_posts) AND (SELECT count(*)=1 FROM sessions)`).Scan(&correct); err != nil || !correct {
		t.Fatalf("private hint changed core/content: %v %v", correct, err)
	}
}

func TestSlackRejectedFeedbackCutoffsAndExactControlDestination(t *testing.T) {
	for _, scenario := range []string{"control", "disconnected", "authorization_lost", "reauthorized", "publication_revoked", "publication_reconfigured", "expired_ingress", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStatusFixture(t)
			id := f.reply(t, "rejected", false)
			if scenario == "control" {
				id = f.controlReceipt(t, f.control("stop"), nil)
			}
			if scenario == "expired_ingress" {
				raw, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Text: "private input"})
				r, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "expired", Actor: "human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(-time.Second), Payload: raw})
				if err != nil {
					t.Fatal(err)
				}
				id = r.ID
			} else if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
				t.Fatal(err)
			}
			// Leave only the exact chosen receipt eligible.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=NULL,payload_expired_at=clock_timestamp() WHERE status='rejected' AND id<>$1`, id)
			switch scenario {
			case "disconnected":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET disconnected_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, f.installation)
			case "authorization_lost":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			case "reauthorized":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=clock_timestamp() WHERE id=$1`, f.installation)
			case "publication_revoked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "publication_reconfigured":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET created_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "malformed":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=convert_to('{"unknown":true}','UTF8') WHERE id=$1`, id)
			}
			intent, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
			if err != nil || !found {
				t.Fatalf("consume: %v %v", found, err)
			}
			var disposed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT payload IS NULL AND payload_expired_at IS NOT NULL AND status='rejected' AND request_digest IS NOT NULL AND error IS NOT NULL FROM slack_requests WHERE id=$1`, id).Scan(&disposed); err != nil || !disposed {
				t.Fatalf("content survived disposition: %v %v", disposed, err)
			}
			want := scenario == "control" || scenario == "expired_ingress"
			if (intent != nil) != want {
				t.Fatalf("intent=%+v want=%v", intent, want)
			}
			if intent != nil {
				var payload struct {
					Channel string `json:"channel"`
					User    string `json:"user"`
				}
				if err := json.Unmarshal(intent.payload, &payload); err != nil || payload.Channel != "C1" || payload.User != "human" {
					t.Fatalf("destination: %+v %v", payload, err)
				}
			}
			if _, again, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err != nil || again {
				t.Fatalf("disposed receipt repeated after crash: %v %v", again, err)
			}
		})
	}
}

func TestSlackRejectedContentExpiryPreservesEventDeduplicationAndConflicts(t *testing.T) {
	f := newStatusFixture(t)
	handler := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	event := map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> PRIVATE_REJECTED_INPUT"}
	send := func(key string, want int) {
		t.Helper()
		if response := serveSignedEvent(handler, eventBody(t, key, event)); response.Code != want {
			t.Fatalf("event %s: status=%d want=%d", key, response.Code, want)
		}
	}
	send("original", http.StatusOK)
	var id uuid.UUID
	var digest []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,request_digest FROM slack_requests WHERE request_key='original'`).Scan(&id, &digest); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err != nil || !found {
		t.Fatalf("dispose: %v %v", found, err)
	}
	f.link(t) // Identity restoration cannot revive a rejected operation.
	send("original", http.StatusOK)
	send("same-source-new-event-id", http.StatusOK)
	event["text"] = "<@bot> changed input"
	send("original", http.StatusConflict)
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*)=1 FROM slack_requests) AND (SELECT id=$1 AND request_digest=$2 AND payload IS NULL AND payload_expired_at IS NOT NULL AND status='rejected' AND error='identity_unlinked' FROM slack_requests) AND NOT EXISTS(SELECT 1 FROM turns)`, id, digest).Scan(&correct); err != nil || !correct {
		t.Fatalf("retained receipt changed or replayed: %v %v", correct, err)
	}
}

func TestSlackRejectedContentDisposalRollsBackAndLeavesOtherOwnersAlone(t *testing.T) {
	f := newStatusFixture(t)
	rejected := f.reply(t, "rejected", false)
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), rejected); err != nil {
		t.Fatal(err)
	}
	pending := f.reply(t, "pending", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, pending)
	f.link(t)
	accepted := f.reply(t, "accepted", false)
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), accepted); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests ADD CONSTRAINT fixture_keep_payload CHECK(payload IS NOT NULL)`)
	if intent, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err == nil || found || intent != nil {
		t.Fatalf("failed disposal escaped transaction: %v %v %v", intent, found, err)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(payload IS NOT NULL AND payload_expired_at IS NULL) FROM slack_requests`).Scan(&retained); err != nil || !retained {
		t.Fatalf("rollback lost input: %v %v", retained, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests DROP CONSTRAINT fixture_keep_payload`)
	if _, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err != nil || !found {
		t.Fatalf("retry: %v %v", found, err)
	}
	if _, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig()); err != nil || found {
		t.Fatalf("other owner's content consumed: %v %v", found, err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='rejected' AND payload IS NULL AND payload_expired_at IS NOT NULL FROM slack_requests WHERE id=$1) AND (SELECT status='received' AND payload IS NOT NULL AND payload_expired_at IS NULL FROM slack_requests WHERE id=$2) AND (SELECT status='accepted' AND payload IS NOT NULL AND payload_expired_at IS NULL FROM slack_requests WHERE id=$3)`, rejected, pending, accepted).Scan(&correct); err != nil || !correct {
		t.Fatalf("disposal crossed retention owner: %v %v", correct, err)
	}
}

func TestSlackUnknownRejectionUsesPrivateAccessFeedback(t *testing.T) {
	f := newStatusFixture(t)
	id := f.reply(t, "unknown-rejection", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error='unknown_rejection' WHERE id=$1`, id)
	client := deliveryTestClient(func(r *http.Request) string {
		var body struct {
			Channel string            `json:"channel"`
			User    string            `json:"user"`
			Text    string            `json:"text"`
			Blocks  []json.RawMessage `json:"blocks"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Text, "Check your access in Console, then try again.") {
			t.Fatal(body.Text)
		}
		return `{"ok":true,"message_ts":"123.999"}`
	})
	if n, err := ReconcileRejectedFeedback(t.Context(), f.Pool, client, testProjectionConfig(), 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
