package slack

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// unboundOpening starts a thread without a confirmed root, with an opening and a
// reply queued behind it, and leaves the opening's first send uncertain.
func (f statusFixture) unboundOpening(t *testing.T) (opening, reply uuid.UUID, first PostClaim) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
	opening = f.opening(t)
	reply = f.post(t, 2, "later", "lifecycle", nil)
	first = f.postClaim(t, opening)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	return opening, reply, first
}

func (f statusFixture) resent(t *testing.T) bool {
	t.Helper()
	var resent bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT opening_resent_at IS NOT NULL FROM slack_threads WHERE id=$1`, f.thread).Scan(&resent); err != nil {
		t.Fatal(err)
	}
	return resent
}

func TestSlackHistoryTransientFailuresRetryWithoutDisposal(t *testing.T) {
	for _, mode := range []string{"unavailable", "missing-scope", "credential", "warnings"} {
		t.Run(mode, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			message := observedClaim(t, claim)
			calls := 0
			client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) {
				if mode == "credential" && calls < 3 {
					calls++
					return "", errCredentialUnavailable
				}
				return "fixture", nil
			}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatal("uncertain mutation resent")
				}
				if calls >= 4 {
					encoded, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{message}})
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
				}
				switch mode {
				case "unavailable":
					return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				case "missing-scope":
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"missing_scope"}`))}, nil
				case "warnings":
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[],"response_metadata":{"warnings":["superfluous_charset"]}}`))}, nil
				}
				t.Fatalf("unexpected read in %s", mode)
				return nil, nil
			}))
			for range 3 {
				f.historyDue(t, post)
				if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did {
					t.Fatal("bounded check", did, err)
				}
				var waiting bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT status='uncertain' AND delivery_disposed_at IS NULL AND next_attempt_at>clock_timestamp()+interval '50 seconds' FROM slack_posts WHERE id=$1`, post).Scan(&waiting); err != nil || !waiting {
					t.Fatal("transient read failure ended the check", waiting, err)
				}
			}
			f.historyDue(t, post)
			if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did {
				t.Fatal("confirming check", did, err)
			}
			var posted bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND delivery_disposed_at IS NULL AND message_ts='123.789' AND reconciliation_pages=4 FROM slack_posts WHERE id=$1`, post).Scan(&posted); err != nil || !posted {
				t.Fatal("retried check did not confirm delivery", posted, err)
			}
		})
	}
}

func TestSlackHistoryInvalidResponseStillTerminates(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"messages":[]}`))}, nil
	}))
	f.historyDue(t, post)
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did {
		t.Fatal(did, err)
	}
	var disposed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='invalid_history_response' AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1`, post).Scan(&disposed); err != nil || !disposed {
		t.Fatal("invalid response retried", disposed, err)
	}
}

func TestSlackUnconfirmedOpeningIsSentOnceMoreAndRepliesFollowNewRoot(t *testing.T) {
	for _, outcome := range []string{"no-match", "budget"} {
		t.Run(outcome, func(t *testing.T) {
			f := newStatusFixture(t)
			opening, reply, first := f.unboundOpening(t)
			if outcome == "no-match" {
				if err := f.finishUnknown(t, opening); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET reconciliation_pages=$2 WHERE id=$1`, opening, historyPageBudget)
				if err := f.finishUnknown(t, opening); err != nil {
					t.Fatal(err)
				}
			}
			reason := map[string]string{"no-match": "history_no_match", "budget": "history_attempt_limit"}[outcome]
			var reset bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND error=$2 AND delivery_disposed_at IS NULL AND claimed_until IS NULL AND inflight_payload IS NULL AND inflight_attempt_id IS NULL AND desired_revision=1 AND confirmed_revision=0 AND suppressed_revision=0 FROM slack_posts WHERE id=$1`, opening, reason).Scan(&reset); err != nil || !reset {
				t.Fatal("unconfirmed opening not returned to pending", reset, err)
			}
			if !f.resent(t) {
				t.Fatal("resend not recorded on the thread")
			}
			var waiting bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND delivery_disposed_at IS NULL FROM slack_posts WHERE id=$1`, reply).Scan(&waiting); err != nil || !waiting {
				t.Fatal("queued reply was disposed", waiting, err)
			}
			f.due(t)
			f.noPostClaim(t, reply)
			second := f.postClaim(t, opening)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(second.Payload, &payload); err != nil || payload["thread_ts"] != nil {
				t.Fatalf("replacement opening is not a root: %s %v", second.Payload, err)
			}
			if second.AttemptID == first.AttemptID || second.AttemptCount != 2 || string(payload["metadata"]) != string(mustField(t, first.Payload, "metadata")) {
				t.Fatalf("replacement attempt identity: %+v", second)
			}
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, second, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.900"}); err != nil {
				t.Fatal(err)
			}
			var bound bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT t.thread_ts='123.900' AND p.status='posted' AND p.message_ts='123.900' FROM slack_threads t JOIN slack_posts p ON p.id=$2 WHERE t.id=$1`, f.thread, opening).Scan(&bound); err != nil || !bound {
				t.Fatal("replacement root not bound", bound, err)
			}
			f.due(t)
			later := f.postClaim(t, reply)
			if err := json.Unmarshal(later.Payload, &payload); err != nil || string(payload["thread_ts"]) != `"123.900"` {
				t.Fatalf("queued reply did not follow the replacement root: %s %v", later.Payload, err)
			}
		})
	}
}

func mustField(t *testing.T, raw []byte, field string) json.RawMessage {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload[field]
}

func TestSlackUnconfirmedReplacementOpeningEndsThreadDelivery(t *testing.T) {
	f := newStatusFixture(t)
	opening, reply, _ := f.unboundOpening(t)
	if err := f.finishUnknown(t, opening); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	second := f.postClaim(t, opening)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, second, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	if err := f.finishUnknown(t, opening); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, opening)
	f.noPostClaim(t, reply)
	var disposed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT o.status='failed' AND o.error='history_no_match' AND o.delivery_disposed_at IS NOT NULL AND o.inflight_attempt_id=$3 AND o.attempt_count=2 AND r.status='suppressed' AND t.thread_ts IS NULL
 FROM slack_posts o, slack_posts r, slack_threads t WHERE o.id=$1 AND r.id=$2 AND t.id=o.thread_id`, opening, reply, second.AttemptID).Scan(&disposed); err != nil || !disposed {
		t.Fatal("second unconfirmed opening was sent again", disposed, err)
	}
}

func TestSlackUnverifiableOpeningDisposesWithoutReplacement(t *testing.T) {
	f := newStatusFixture(t)
	opening, reply, _ := f.unboundOpening(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
	f.historyDue(t, opening)
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("unverifiable route read history")
		return nil, nil
	}))
	if _, err := ReconcileUnknownPost(t.Context(), f.Pool, client, opening); err != nil {
		t.Fatal(err)
	}
	var disposed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='delivery_unverifiable' AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1`, opening).Scan(&disposed); err != nil || !disposed {
		t.Fatal("unverifiable opening not disposed", disposed, err)
	}
	if f.resent(t) {
		t.Fatal("unverifiable opening was sent again")
	}
	f.due(t)
	f.noPostClaim(t, opening)
	f.noPostClaim(t, reply)
}

// supersededRootClient serves one managed root message carrying the opening's
// metadata, sent by the fixture's own app.
func (f statusFixture) supersededRootClient(t *testing.T, opening uuid.UUID, reads *int) *WebClient {
	t.Helper()
	admission := f.admissionClient(t)
	return NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/conversations.history" {
			return admission.http.Do(r)
		}
		*reads++
		raw, _ := json.Marshal(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": r.URL.Query().Get("latest"), "user": "bot", "app_id": "app", "text": "Working on it",
			"metadata": map[string]any{"event_type": "helmr_post", "event_payload": map[string]any{"post_id": opening.String(), "revision": 1}}}}})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}))
}

func TestSlackReplyUnderSupersededRootIsRejectedImmediately(t *testing.T) {
	for _, kind := range []string{"bound-elsewhere", "abandoned"} {
		t.Run(kind, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.link(t)
			var opening uuid.UUID
			if kind == "bound-elsewhere" {
				opening = f.opening(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='posted',posted_at=clock_timestamp(),message_ts='123.456',confirmed_revision=desired_revision WHERE id=$1`, opening)
			} else {
				opening, _, _ = f.unboundOpening(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET reconciliation_pages=$2 WHERE id=$1`, opening, historyPageBudget)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET opening_resent_at=clock_timestamp() WHERE id=$1`, f.thread)
				if err := f.finishUnknown(t, opening); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			client := f.supersededRootClient(t, opening, &reads)
			id := f.newMessage(t, "orphan-reply", "123.111")
			if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, nil, client, id); err != nil {
				t.Fatal(err)
			}
			var rejected bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='rejected' AND r.error='thread_superseded' AND (SELECT count(*) FROM sessions WHERE environment_id=$2)=1 FROM slack_requests r WHERE r.id=$1`, id, f.Environment).Scan(&rejected); err != nil || !rejected || reads != 1 {
				t.Fatal("reply under a superseded root was not rejected at once", rejected, reads, err)
			}
			var root *string
			if err := f.Pool.QueryRow(t.Context(), `SELECT thread_ts FROM slack_threads WHERE id=$1`, f.thread).Scan(&root); err != nil || (kind == "abandoned") != (root == nil) {
				t.Fatal("superseded reply rebound the thread", root, err)
			}
		})
	}
}

func TestSlackDeliveryStoppedAttentionListsOpenSessionsOnly(t *testing.T) {
	f := newStatusFixture(t)
	caller := session.UserCaller(f.Principal(t))
	list := func() session.Attention {
		t.Helper()
		result, err := session.ListAttention(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, f.Environment)
		if err != nil {
			t.Fatal(err)
		}
		counts, err := session.CountAttention(t.Context(), f.Pool, caller, f.Environment)
		if err != nil || counts.SlackDeliveryStoppedCount != result.SlackDeliveryStoppedCount {
			t.Fatal("count/list mismatch", counts, result.SlackDeliveryStoppedCount, err)
		}
		return result
	}
	opening, _, _ := f.unboundOpening(t)
	if got := list(); got.SlackDeliveryStoppedCount != 0 || len(got.SlackDeliveryStopped) != 0 {
		t.Fatal("uncertain opening listed before its checks ended", got.SlackDeliveryStopped)
	}
	if err := f.finishUnknown(t, opening); err != nil {
		t.Fatal(err)
	}
	if got := list(); got.SlackDeliveryStoppedCount != 0 {
		t.Fatal("opening sent again listed as stopped", got.SlackDeliveryStopped)
	}
	f.due(t)
	second := f.postClaim(t, opening)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, second, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	if err := f.finishUnknown(t, opening); err != nil {
		t.Fatal(err)
	}
	got := list()
	if got.SlackDeliveryStoppedCount != 1 || len(got.SlackDeliveryStopped) != 1 {
		t.Fatalf("disposed opening not listed: %+v", got.SlackDeliveryStopped)
	}
	if item := got.SlackDeliveryStopped[0]; item.ID != opening || item.SessionID != f.Session || item.Agent != "agent" || item.Summary != "history_no_match" || item.TurnID != nil || time.Since(item.At) > time.Minute {
		t.Fatalf("stopped delivery item: %+v", item)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Session)
	if got := list(); got.SlackDeliveryStoppedCount != 0 || len(got.SlackDeliveryStopped) != 0 {
		t.Fatal("closed Session still listed", got.SlackDeliveryStopped)
	}
}
