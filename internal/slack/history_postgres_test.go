package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func (f statusFixture) historyDue(t *testing.T, post uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET history_next_at=clock_timestamp() WHERE id=$1;
 UPDATE slack_posts SET next_attempt_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.installation, post)
}

func TestSlackHistoryReconciliationPersistsCursorAndUsesPositiveEvidence(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	message := observedClaim(t, claim)
	calls := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"next-page"}}`
		if calls == 2 {
			if r.URL.Query().Get("cursor") != "next-page" {
				t.Fatal("persisted cursor not used")
			}
			encoded, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{message}})
			body = string(encoded)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !read {
		t.Fatalf("first page: %v %v", read, err)
	}
	var still bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='uncertain' AND reconciliation_cursor='next-page' FROM slack_posts WHERE id=$1`, post).Scan(&still); err != nil || !still {
		t.Fatalf("absence changed disposition/cursor lost: %v %v", still, err)
	}
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || read || calls != 1 {
		t.Fatalf("history budget bypassed: %v %d %v", read, calls, err)
	}
	f.historyDue(t, post)
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !read || calls != 2 {
		t.Fatalf("second page: %v %d %v", read, calls, err)
	}
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND message_ts='123.789' FROM slack_posts WHERE id=$1`, post).Scan(&posted); err != nil || !posted {
		t.Fatalf("authenticated proof not applied: %v %v", posted, err)
	}
}

func TestSlackHistoryRateLimitIsSharedAndDoesNotReplayMutation(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet {
			t.Fatal("uncertain mutation replayed")
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !read {
		t.Fatalf("rate read: %v %v", read, err)
	}
	var delay float64
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch from history_next_at-clock_timestamp()) FROM slack_installations WHERE id=$1`, f.installation).Scan(&delay); err != nil || delay < 115 {
		t.Fatalf("shared history cooldown missing: %v %v", delay, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET next_attempt_at=clock_timestamp() WHERE id=$1`, post)
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || read || calls != 1 {
		t.Fatalf("installation cooldown ignored: %v %d %v", read, calls, err)
	}
	f.due(t)
	f.noPostClaim(t, post)
}

func TestSlackHistoryExpiredCursorPausesWithoutDeliveryClaim(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET reconciliation_cursor='expired' WHERE id=$1`, post)
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"invalid_cursor"}`))}, nil
	}))
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !read {
		t.Fatalf("cursor error: %v %v", read, err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='uncertain' AND reconciliation_cursor='' AND reconciliation_paused_at IS NOT NULL AND inflight_payload=$2 FROM slack_posts WHERE id=$1`, post, claim.Payload).Scan(&exact); err != nil || !exact {
		t.Fatalf("cursor recovery changed delivery: %v %v", exact, err)
	}
}

func TestSlackHistoryConfirmationReleasesNewerDeliveryWithoutReadCooldown(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"text":"new content"}`)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET desired_revision=2,payload=$2,payload_digest=$3 WHERE id=$1`, post, body, digest[:])
	message := observedClaim(t, claim)
	encoded, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{message}})
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	}))
	f.due(t)
	if read, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !read {
		t.Fatalf("positive read: %v %v", read, err)
	}
	update := f.postClaim(t, post)
	if update.Method != "chat.update" || update.Revision != 2 {
		t.Fatalf("new delivery blocked by read pacing: %+v", update)
	}
	var paced bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT history_next_at>clock_timestamp() FROM slack_installations WHERE id=$1`, f.installation).Scan(&paced); err != nil || !paced {
		t.Fatalf("read pacing lost: %v %v", paced, err)
	}
}
