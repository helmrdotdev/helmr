package slack

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func deleteEvent(t *testing.T, f statusFixture, timestamp string) {
	t.Helper()
	h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
	body := eventBody(t, "deleted", map[string]any{"type": "message", "subtype": "message_deleted", "hidden": true, "channel": "C1", "deleted_ts": timestamp, "ts": slackNow()})
	for range 2 {
		if r := serveSignedEvent(h, body); r.Code != http.StatusOK {
			t.Fatalf("deletion callback: %d", r.Code)
		}
	}
}

func TestSlackDeletedRootSuppressesFutureDeliveryButPreservesCoreAndUncertainty(t *testing.T) {
	f := newStatusFixture(t)
	unknown := f.post(t, 1, "unknown", "lifecycle", nil)
	claim := f.postClaim(t, unknown)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	pending := f.post(t, 2, "pending", "lifecycle", nil)
	f.due(t)
	status := f.claim(t, "active")
	deleteEvent(t, f, "123.456")
	if allowed, err := statusClaimAuthorized(t.Context(), f.Pool, status); err != nil || allowed {
		t.Fatalf("deleted status claim remains authorized: %v %v", allowed, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, status, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.noPostClaim(t, pending)
	later := f.post(t, 3, "after-deletion", "lifecycle", nil)
	f.due(t)
	f.noPostClaim(t, later)
	if c, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || c != nil {
		t.Fatalf("deleted root status: %v %v", c, err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT
 (SELECT deleted_at IS NOT NULL AND thread_ts='123.456' AND delivery_error='root_deleted' AND status_confirmation='unknown' FROM slack_threads WHERE id=$1)
 AND (SELECT status='uncertain' AND inflight_attempt_id IS NOT NULL FROM slack_posts WHERE id=$2)
 AND (SELECT bool_and(status='suppressed' AND error='root_deleted') FROM slack_posts WHERE id IN ($3,$4))`, f.thread, unknown, pending, later).Scan(&correct); err != nil || !correct {
		t.Fatalf("deletion lost evidence or revived root: %v %v", correct, err)
	}
	if _, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "after-delete", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatalf("projection deletion cancelled core: %v", err)
	}
}

func TestSlackDeletedIndividualPostCannotBeRevivedByLateMutationAck(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	first := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='pending',desired_revision=2 WHERE id=$1`, post)
	f.due(t)
	update := f.postClaim(t, post)
	deleteEvent(t, f, "123.789")
	if err := FinishPost(t.Context(), f.Pool, update, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	f.noPostClaim(t, post)
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='failed' AND error='message_deleted' AND message_ts='123.789' FROM slack_posts WHERE id=$1) AND (SELECT deleted_at IS NULL FROM slack_threads WHERE id=$2)`, post, f.thread).Scan(&correct); err != nil || !correct {
		t.Fatalf("individual deletion disposition: %v %v", correct, err)
	}
	f.due(t)
	next := f.post(t, 2, "later", "lifecycle", nil)
	f.postClaim(t, next)
}

func TestSlackDeletionRequiresAuthenticatedExactWorkspaceAndKnownIdentity(t *testing.T) {
	for _, kind := range []string{"unsigned", "team", "channel", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			h := EventHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret")}
			channel, timestamp := "C1", "123.456"
			if kind == "channel" {
				channel = "C2"
			}
			if kind == "unknown" {
				timestamp = "456.789"
			}
			body := eventBody(t, "deleted", map[string]any{"type": "message", "subtype": "message_deleted", "channel": channel, "deleted_ts": timestamp})
			if kind == "team" {
				body = bytes.Replace(body, []byte(`"team_id":"team"`), []byte(`"team_id":"other"`), 1)
			}
			if kind == "unsigned" {
				r := httptest.NewRecorder()
				h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body)))
				if r.Code != http.StatusUnauthorized {
					t.Fatalf("unsigned deletion: %d", r.Code)
				}
			} else if r := serveSignedEvent(h, body); r.Code != http.StatusOK {
				t.Fatalf("callback: %d", r.Code)
			}
			var untouched bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT deleted_at IS NULL FROM slack_threads WHERE id=$1`, f.thread).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("wrong deletion applied: %v %v", untouched, err)
			}
		})
	}
}
