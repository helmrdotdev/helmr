package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func (f statusFixture) post(t *testing.T, seq int64, key, role string, turn *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	body := []byte(`{"text":"hello","mrkdwn":false,"parse":"none"}`)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,publication_key,role,turn_id,payload,payload_digest,presentation_path) VALUES($1,$2,$3,$4,(SELECT thread_id FROM slack_thread_sources WHERE id=$4),$5,$6,$7,$8,$9,$10,'post')`, id, f.Environment, f.Session, f.participant, seq, key, role, turn, body, digest[:])
	return id
}
func (f statusFixture) opening(t *testing.T) uuid.UUID {
	t.Helper()
	turn, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "opening-fixture", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	return f.post(t, 1, "opening", "opening", &turn.TurnID)
}

func (f statusFixture) postClaim(t *testing.T, id uuid.UUID) PostClaim {
	t.Helper()
	c, err := ClaimPost(t.Context(), f.Pool, id)
	if err != nil || c == nil {
		t.Fatalf("claim post: %+v %v", c, err)
	}
	return *c
}
func (f statusFixture) noPostClaim(t *testing.T, id uuid.UUID) {
	t.Helper()
	c, err := ClaimPost(t.Context(), f.Pool, id)
	if err != nil || c != nil {
		t.Fatalf("unexpected post claim: %+v %v", c, err)
	}
}

func TestSlackPostUnknownOpeningBlocksDependentContentWithoutReplay(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
	opening := f.opening(t)
	reply := f.post(t, 2, "later", "lifecycle", nil)
	f.noPostClaim(t, reply)
	c := f.postClaim(t, opening)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(c.Payload, &payload); err != nil || payload["thread_ts"] != nil || payload["metadata"] == nil {
		t.Fatalf("opening payload: %s %v", c.Payload, err)
	}
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, opening)
	f.noPostClaim(t, reply)
	// A stale successful completion cannot convert an uncertain attempt to posted.
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	var remains bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.status='uncertain' AND t.thread_ts IS NULL FROM slack_posts p JOIN slack_threads t ON t.id=$2 WHERE p.id=$1`, opening, f.thread).Scan(&remains); err != nil || !remains {
		t.Fatalf("unknown opening lost: %v %v", remains, err)
	}
}

func TestSlackPostOpeningAckAllowsReplyAndExpiredClaimStaysUncertain(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
	opening := f.opening(t)
	reply := f.post(t, 2, "later", "lifecycle", nil)
	c := f.postClaim(t, opening)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	r := f.postClaim(t, reply)
	var payload map[string]any
	if err := json.Unmarshal(r.Payload, &payload); err != nil || payload["thread_ts"] != "123.789" || payload["channel"] != "C1" {
		t.Fatalf("reply destination: %s %v", r.Payload, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, reply)
	f.due(t)
	f.noPostClaim(t, reply)
	f.noPostClaim(t, reply)
	var state string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_posts WHERE id=$1`, reply).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("expired mutation replayed: %s %v", state, err)
	}
}

func TestSlackPostRateLimitRetriesFrozenRevisionBeforeNewerContent(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	first := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: RateLimited, RetryAfter: 90 * time.Second}); err != nil {
		t.Fatal(err)
	}
	f.noPostClaim(t, post)
	body := []byte(`{"text":"new content"}`)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET desired_revision=2,payload=$2,payload_digest=$3,next_attempt_at=clock_timestamp() WHERE id=$1`, post, body, digest[:])
	f.due(t)
	retry := f.postClaim(t, post)
	if retry.Revision != 1 || !bytes.Equal(first.Payload, retry.Payload) || retry.AttemptID == first.AttemptID {
		t.Fatal("rate retry changed exact mutation")
	}
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	if err := FinishPost(t.Context(), f.Pool, retry, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	update := f.postClaim(t, post)
	if update.Method != "chat.update" || update.Revision != 2 || !bytes.Contains(update.Payload, []byte("new content")) {
		t.Fatalf("new revision missing: %+v", update)
	}
	if err := FinishPost(t.Context(), f.Pool, update, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var done bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND confirmed_revision=2 AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&done); err != nil || !done {
		t.Fatalf("update incomplete: %v %v", done, err)
	}
}

func TestSlackPostUncertainProgressBlocksDependentsButAllowsExactQuestion(t *testing.T) {
	f := newStatusFixture(t)
	turn, ask := f.pendingQuestion(t)
	progress := f.post(t, 1, "progress", "intermediate", &turn)
	continuation := f.post(t, 2, "progress-next", "intermediate", &turn)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET publication_key='progress',continuation_ordinal=1 WHERE id=$1`, continuation)
	response := f.post(t, 3, "response", "response", &turn)
	question := f.post(t, 4, "attention", "lifecycle", nil)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET role='question',turn_id=$2,ask_id=$3 WHERE id=$1`, question, turn, ask)
	f.noPostClaim(t, response)
	p := f.postClaim(t, progress)
	if err := FinishPost(t.Context(), f.Pool, p, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.noPostClaim(t, continuation)
	f.noPostClaim(t, response)
	q := f.postClaim(t, question)
	if err := FinishPost(t.Context(), f.Pool, q, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.100"}); err != nil {
		t.Fatal(err)
	}
	// An explicit unsuccessful disposition releases the dependency; the network
	// outcome becoming unknown did not itself authorize that disposition.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='failed',error='delivery_abandoned',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL WHERE id=$1`, progress)
	f.due(t)
	next := f.postClaim(t, continuation)
	if err := FinishPost(t.Context(), f.Pool, next, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.101"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.postClaim(t, response)
}

func TestSlackPostRechecksPublicationAndAuthorizationLoss(t *testing.T) {
	for _, kind := range []string{"publication", "authorization"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			if kind == "publication" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			}
			f.noPostClaim(t, post)
			var state string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_posts WHERE id=$1`, post).Scan(&state); err != nil || state != "suppressed" {
				t.Fatalf("stale authority not suppressed: %s %v", state, err)
			}
		})
	}
}

type callerFunc func(context.Context, uuid.UUID, int64, string, []byte) DeliveryResult

func (f callerFunc) VerifyChannel(context.Context, uuid.UUID, int64, string, string) error {
	return nil
}

func (f callerFunc) Call(ctx context.Context, installation uuid.UUID, credential int64, method string, payload []byte) DeliveryResult {
	return f(ctx, installation, credential, method, payload)
}

func TestSlackPostRechecksClaimBeforeHTTP(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if allowed, err := postClaimAuthorized(t.Context(), f.Pool, claim); err != nil || !allowed {
		t.Fatalf("fresh claim not authorized: %v %v", allowed, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
	if allowed, err := postClaimAuthorized(t.Context(), f.Pool, claim); err != nil || allowed {
		t.Fatalf("revoked claim authorized: %v %v", allowed, err)
	}
}

func TestSlackPostHTTPRunsAfterFrozenClaimCommits(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	calls := 0
	client := callerFunc(func(ctx context.Context, installation uuid.UUID, credential int64, method string, payload []byte) DeliveryResult {
		calls++
		if installation != f.installation || credential != 1 || method != "chat.postMessage" {
			t.Fatal("wrong delivery destination")
		}
		var exact bool
		if err := f.Pool.QueryRow(ctx, `SELECT status='sending' AND inflight_payload=$2 FROM slack_posts WHERE id=$1`, post, payload).Scan(&exact); err != nil || !exact {
			t.Fatalf("HTTP before durable claim: %v %v", exact, err)
		}
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, installation).Scan(&id); err != nil {
			t.Fatalf("HTTP held installation lock: %v", err)
		}
		return DeliveryResult{Disposition: Acknowledged, Timestamp: "123.100"}
	})
	if sent, err := ReconcilePost(t.Context(), f.Pool, client, post); err != nil || !sent || calls != 1 {
		t.Fatalf("delivery: %v %d %v", sent, calls, err)
	}
	if sent, err := ReconcilePost(t.Context(), f.Pool, client, post); err != nil || sent || calls != 1 {
		t.Fatalf("posted content resent: %v %d %v", sent, calls, err)
	}
}

func TestSlackPostKnownRootContainsOpeningPublication(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "opening", "lifecycle", nil)
	claim := f.postClaim(t, post)
	var payload map[string]any
	if err := json.Unmarshal(claim.Payload, &payload); err != nil || payload["thread_ts"] != "123.456" {
		t.Fatalf("opening escaped existing root: %s %v", claim.Payload, err)
	}
}

func TestSlackPostFreshUpdateCanUseMessageFromOlderAuthorization(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "card", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	// The reauthorization boundary suppresses pending intents, not retained posted
	// identities. This later source is eligible for a new revision of the same card.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=clock_timestamp() WHERE id=$1`, f.installation)
	body := []byte(`{"text":"fresh update"}`)
	digest := sha256.Sum256(body)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status='pending',desired_revision=2,payload=$2,payload_digest=$3 WHERE id=$1`, post, body, digest[:])
	f.due(t)
	update := f.postClaim(t, post)
	if update.Method != "chat.update" || update.Revision != 2 || !bytes.Contains(update.Payload, []byte("fresh update")) {
		t.Fatalf("fresh revision excluded old identity: %+v", update)
	}
}

func TestSlackPostAuthorizationLossSuppressesPendingButPreservesUnknown(t *testing.T) {
	f := newStatusFixture(t)
	unknown := f.post(t, 1, "unknown", "lifecycle", nil)
	c := f.postClaim(t, unknown)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	pending := f.post(t, 2, "pending", "lifecycle", nil)
	failure := f.post(t, 3, "failure", "lifecycle", nil)
	f.due(t)
	c = f.postClaim(t, failure)
	if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Rejected, Code: "token_revoked"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]string{unknown: "uncertain", pending: "suppressed", failure: "failed"} {
		var state string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM slack_posts WHERE id=$1`, id).Scan(&state); err != nil || state != want {
			t.Fatalf("authorization loss disposition: %s want %s: %v", state, want, err)
		}
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_payload IS NOT NULL AND inflight_attempt_id IS NOT NULL FROM slack_posts WHERE id=$1`, unknown).Scan(&retained); err != nil || !retained {
		t.Fatalf("unknown evidence lost: %v %v", retained, err)
	}
}

func TestDefinitivelyUnavailableOpeningSettlesDependentProjection(t *testing.T) {
	for _, kind := range []string{"rejected", "suppressed-then-repaired"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
			write := f.outputWriter(t)
			opening := f.opening(t)
			write("content before opening disposition")
			f.projectAll(t)
			if kind == "rejected" {
				claim := f.postClaim(t, opening)
				if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Rejected, Code: "invalid_blocks"}); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
				f.noPostClaim(t, opening)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=NULL,authorized_at=clock_timestamp() WHERE id=$1`, f.installation)
			}
			// Existing pending content must be discoverable for terminal cleanup.
			for range 5 {
				candidates, err := deliveryCandidates(t.Context(), f.Pool, 64)
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range candidates {
					if candidate.kind == "post" {
						f.noPostClaim(t, candidate.id)
					}
				}
			}
			var settled bool
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(status IN ('failed','suppressed') AND inflight_attempt_id IS NULL) FROM slack_posts WHERE thread_id=$1`, f.thread).Scan(&count, &settled); err != nil || !settled || count < 2 {
				t.Fatal("unavailable root stranded pending content", count, settled, err)
			}
			write("content after opening disposition")
			f.projectAll(t)
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_posts WHERE thread_id=$1)=$2 AND t.thread_ts IS NULL AND src.projected_event_seq=s.next_event_seq-1 FROM slack_threads t JOIN slack_thread_sources src ON src.thread_id=t.id JOIN sessions s ON s.environment_id=src.environment_id AND s.id=src.session_id WHERE t.id=$1`, f.thread, count).Scan(&settled); err != nil || !settled {
				t.Fatal("future projection recreated unavailable conversation", settled, err)
			}
		})
	}
}
