package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Exercise the production bounded resolver; no manual recovery API or bypass.
func (f statusFixture) finishUnknown(t *testing.T, post uuid.UUID) error {
	t.Helper()
	f.historyDue(t, post)
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("uncertain mutation resent")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[]}`))}, nil
	}))
	_, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post)
	return err
}

func TestSlackDeliveryTerminationClosesPublicationWithoutReplacingRootOrReplaying(t *testing.T) {
	for _, unbound := range []bool{false, true} {
		t.Run(fmt.Sprintf("unbound=%v", unbound), func(t *testing.T) {
			f := newStatusFixture(t)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
			if unbound {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
			}
			write := f.outputWriter(t)
			if unbound {
				var openingTurn uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&openingTurn); err != nil {
					t.Fatal(err)
				}
				f.post(t, 1, "opening", "opening", &openingTurn)
			}
			write(strings.Repeat("source", 700))
			f.projectAll(t)
			first := f.firstPost(t)
			claim := f.postClaim(t, first)
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT turn_id FROM slack_posts WHERE id=$1`, first).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			seq := int64(3)
			if unbound {
				seq = 4
			}
			response := f.post(t, seq, "response", "response", &turn)
			f.due(t)
			if unbound {
				f.noPostClaim(t, response)
			}

			for range 2 {
				if err := f.finishUnknown(t, first); err != nil {
					t.Fatal(err)
				}
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=$2 AND bool_and(delivery_disposed_at IS NOT NULL AND closed_at IS NOT NULL AND suppressed_revision=desired_revision AND status IN ('failed','suppressed')) FROM slack_posts WHERE thread_source_id=$1 AND role=$3`, f.participant, map[bool]int{false: 2, true: 1}[unbound], map[bool]string{false: "intermediate", true: "opening"}[unbound]).Scan(&exact); err != nil || !exact {
				t.Fatal("publication disposition", exact, err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_payload=$2 AND inflight_attempt_id=$3 AND message_ts IS NULL FROM slack_posts WHERE id=$1`, first, claim.Payload, claim.AttemptID).Scan(&exact); err != nil || !exact {
				t.Fatal("uncertain proof lost", exact, err)
			}
			f.due(t)
			f.noPostClaim(t, first)
			if unbound {
				f.noPostClaim(t, response)
				write("new source only")
				f.projectAll(t)
				if err := f.Pool.QueryRow(t.Context(), `SELECT opening_post_key=p.post_key AND thread_ts IS NULL FROM slack_threads t JOIN slack_posts p ON p.id=$2 WHERE t.id=$1`, f.thread, first).Scan(&exact); err != nil || !exact {
					t.Fatal("suppressed opener replaced", exact, err)
				}
			} else {
				c := f.postClaim(t, response)
				if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, c, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !ok {
				t.Fatal("late positive evidence", ok, err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='history_no_match' AND delivery_disposed_at IS NOT NULL AND confirmed_revision=1 AND message_ts='123.789' AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, first).Scan(&exact); err != nil || !exact {
				t.Fatal("late evidence reopened disposition", exact, err)
			}
			f.due(t)
			f.noPostClaim(t, first)
			if unbound {
				f.noPostClaim(t, response)
			}
		})
	}
}

func TestSlackDeliveryHistoryChecksTerminateWithoutSending(t *testing.T) {
	for _, mode := range []string{"empty", "unavailable", "pagination", "page-budget", "rate-limit"} {
		t.Run(mode, func(t *testing.T) {
			f := newStatusFixture(t)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Error("uncertain content replayed")
				}
				body := `{"ok":true,"messages":[]}`
				switch mode {
				case "rate-limit":
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				case "unavailable":
					body = `{"ok":false,"error":"missing_scope"}`
				case "pagination":
					body = `{"ok":true,"messages":[],"has_more":true}`
				case "page-budget":
					body = fmt.Sprintf(`{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"page-%d"}}`, calls)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			want := 1
			if mode == "page-budget" || mode == "rate-limit" {
				want = historyPageBudget
			}
			for range want {
				f.historyDue(t, post)
				if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did {
					t.Fatal("bounded check", did, err)
				}
			}
			f.historyDue(t, post)
			if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || did || calls != want {
				t.Fatal("terminal scan repeated", did, calls, err)
			}
			candidates, err := deliveryCandidates(t.Context(), f.Pool, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range candidates {
				if c.id == post {
					t.Fatal("terminal post monopolizes discovery")
				}
			}
			var safe bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND inflight_payload=$2 AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1`, post, claim.Payload).Scan(&safe); err != nil || !safe {
				t.Fatal("negative evidence changed outcome", safe, err)
			}
			if mode == "rate-limit" {
				var reason string
				if err := f.Pool.QueryRow(t.Context(), `SELECT error FROM slack_posts WHERE id=$1`, post).Scan(&reason); err != nil || reason != "history_rate_limited" {
					t.Fatal("rate limit reason lost", reason, err)
				}
			}
			f.due(t)
			f.noPostClaim(t, post)
		})
	}
}

func TestSlackDeliveryTerminalStreamKeepsProofUntilPositiveEvidenceAndStopsWithoutContent(t *testing.T) {
	for _, method := range []string{"start", "append"} {
		t.Run(method, func(t *testing.T) {
			f := newStatusFixture(t)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
			post := f.streamPost(t, 1, "prefix")
			claim := f.postClaim(t, post)
			text := "prefix"
			if method == "append" {
				if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
					t.Fatal(err)
				}
				f.due(t)
				repair := f.claim(t, "active")
				if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
				text = "prefix appended"
				f.reviseStream(t, post, text)
				f.due(t)
				claim = f.postClaim(t, post)
			}
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			f.reviseStream(t, post, text+" NEVER_PUBLISH")
			if err := f.finishUnknown(t, post); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			f.noPostClaim(t, post)
			var safe bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND stream_status='uncertain' AND inflight_payload=$2 AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1`, post, claim.Payload).Scan(&safe); err != nil || !safe {
				t.Fatal("stream state or proof fabricated", safe, err)
			}
			m := observedStream(t, f, post, text, "in_progress")
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
				t.Fatal("late stream proof", ok, err)
			}
			f.due(t)
			repair := f.claim(t, "active")
			if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			stop := f.postClaim(t, post)
			if stop.Method != "chat.stopStream" || bytes.Contains(stop.Payload, []byte("chunks")) || bytes.Contains(stop.Payload, []byte("NEVER_PUBLISH")) {
				t.Fatal("suppressed bytes or wrong cleanup", stop.Method, string(stop.Payload))
			}
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, stop, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='history_no_match' AND stream_status='stopped' AND delivery_disposed_at IS NOT NULL AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&safe); err != nil || !safe {
				t.Fatal("cleanup reopened disposition", safe, err)
			}
			views, err := ListSessionDelivery(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), f.Environment, f.Session, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(views)
			if !bytes.Contains(raw, []byte(`"stream_status":"stopped"`)) || bytes.Contains(raw, []byte(`"stream_state"`)) {
				t.Fatalf("unexpected stream JSON contract: %s", raw)
			}
			if bytes.Contains(raw, []byte("inflight_payload")) || bytes.Contains(raw, []byte("recipient_user_id")) {
				t.Fatal("transport details exposed")
			}
		})
	}
}

func TestSlackDeliveryTerminationDisposesAcceptedUnprojectedProgress(t *testing.T) {
	f := newStatusFixture(t)
	f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
	write := f.outputWriter(t)
	write("first progress")
	f.projectAll(t)
	post := f.firstPost(t)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}

	write(" accepted before termination")
	if err := f.finishUnknown(t, post); err != nil {
		t.Fatal(err)
	}
	write("new progress after termination")
	f.projectAll(t)
	var raw []byte
	var next uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,payload FROM slack_posts WHERE thread_source_id=$1 AND role<>'controls' AND id<>$2`, f.participant, post).Scan(&next, &raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("new progress after termination")) || bytes.Contains(raw, []byte("first progress")) || bytes.Contains(raw, []byte("accepted before termination")) {
		t.Fatalf("suppressed source replayed: %s", raw)
	}
	f.due(t)
	f.noPostClaim(t, post)
	_ = f.postClaim(t, next)
}

func TestSlackDeliveryNewAttemptStartsItsOwnBoundedReconciliation(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	write("first revision")
	f.projectAll(t)
	post := f.firstPost(t)
	first := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet {
			t.Error("unknown delivery replayed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"next"}}`))}, nil
	}))
	f.historyDue(t, post)
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did || calls != 1 {
		t.Fatal("first attempt check", did, calls, err)
	}
	write(" second revision")
	f.projectAll(t)
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, first)); err != nil || !ok {
		t.Fatal("first attempt evidence", ok, err)
	}
	f.due(t)
	second := f.postClaim(t, post)
	if second.AttemptID == first.AttemptID || second.Revision != 2 || second.Method != "chat.update" {
		t.Fatal("new mutation identity", second)
	}
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, second, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.historyDue(t, post)
	var fresh bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT delivery_disposed_at IS NULL AND reconciliation_pages=0 AND reconciliation_cursor='' FROM slack_posts WHERE id=$1`, post).Scan(&fresh); err != nil || !fresh {
		t.Fatal("new attempt inherited old budget", fresh, err)
	}
	candidates, err := deliveryCandidates(t.Context(), f.Pool, 64)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.id == post {
			found = true
		}
	}
	if !found {
		t.Fatal("new uncertain mutation not discovered")
	}
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did || calls != 2 {
		t.Fatal("new bounded check", did, calls, err)
	}
}

func TestSlackDeliveryTerminationLeavesUnprojectedQuestionsAndResponseIndependent(t *testing.T) {
	for _, kind := range []string{"question", "answered", "response"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
			write := f.outputWriter(t)
			write("suppressed progress")
			f.projectAll(t)
			post := f.firstPost(t)
			first := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT turn_id FROM slack_posts WHERE id=$1`, post).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			execution := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			role := "question"
			if kind == "response" {
				role = "response"
				if err := session.RuntimeRespond(t.Context(), f.Pool, host, execution, turn, uuid.NewV7(), json.RawMessage(`"independent response"`)); err != nil {
					t.Fatal(err)
				}
				if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, f.Pool, host, execution, turn); err != nil {
					t.Fatal(err)
				}
				if _, err := session.RuntimeFinalize(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, host, execution, turn, json.RawMessage(`null`), "callbacks drained"); err != nil {
					t.Fatal(err)
				}

			} else {
				ask := uuid.NewV7()
				if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, f.Pool, host, execution, turn, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Independent question"}],"type":"text"}`)); err != nil {
					t.Fatal(err)
				}
				if kind == "answered" {
					if _, err := session.AnswerAsk(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, AnswerID: "answer", Answer: json.RawMessage(`"answer"`)}); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := f.finishUnknown(t, post); err != nil {
				t.Fatal("independent event blocks disposition", err)
			}
			f.projectAll(t)
			var independent uuid.UUID
			var unaffected bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT id,delivery_disposed_at IS NULL AND status='pending' FROM slack_posts WHERE thread_source_id=$1 AND role=$2`, f.participant, role).Scan(&independent, &unaffected); err != nil || !unaffected {
				t.Fatal("independent publication suppressed", unaffected, err)
			}
			f.due(t)
			_ = f.postClaim(t, independent)
		})
	}
}

func TestSlackDeliveryHistoryWaitsThroughOrdinaryRefresh(t *testing.T) {
	f, store, _ := credentialFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	delivery := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, delivery, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	refresh, err := store.claimRefresh(t.Context(), f.installation)
	if err != nil || refresh == nil {
		t.Fatal("refresh", err)
	}
	calls := 0
	client := NewWebClient(store, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Error("history used obsolete token")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[]}`))}, nil
	}))
	f.historyDue(t, post)
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did || calls != 0 {
		t.Fatal("read during refresh", did, calls, err)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT delivery_disposed_at IS NULL AND reconciliation_pages=1 AND next_attempt_at>clock_timestamp() AND inflight_payload=$2 FROM slack_posts WHERE id=$1`, post, delivery.Payload).Scan(&pending); err != nil || !pending {
		t.Fatal("routine refresh paused evidence", pending, err)
	}
	if err := store.finishRefresh(t.Context(), *refresh, refreshResult()); err != nil {
		t.Fatal(err)
	}
	f.historyDue(t, post)
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did || calls != 1 {
		t.Fatal("read after refresh", did, calls, err)
	}
}

func TestSlackDeliveryHistoryCredentialWaitRemainsBounded(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	delivery := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, delivery, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) {
		attempts++
		return "", errCredentialUnavailable
	}), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("read with unavailable credential")
		return nil, nil
	}))
	for range historyPageBudget {
		f.historyDue(t, post)
		if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did {
			t.Fatal(did, err)
		}
	}
	f.historyDue(t, post)
	if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || did || attempts != historyPageBudget {
		t.Fatal("unbounded credential wait", did, attempts, err)
	}
}

func TestSlackDeliveryTerminationPreservesEarlierFailureReasons(t *testing.T) {
	for _, state := range []string{"failed", "suppressed"} {
		t.Run(state, func(t *testing.T) {
			f := newStatusFixture(t)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
			write := f.outputWriter(t)
			write(strings.Repeat("source", 700))
			f.projectAll(t)
			post := f.firstPost(t)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status=$3,error='stream_content_rejected:invalid_blocks' WHERE thread_source_id=$1 AND role<>'controls' AND id<>$2`, f.participant, post, state)
			if err := f.finishUnknown(t, post); err != nil {
				t.Fatal(err)
			}
			var preserved bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status=$3 AND error='stream_content_rejected:invalid_blocks' AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE thread_source_id=$1 AND role<>'controls' AND id<>$2`, f.participant, post, state).Scan(&preserved); err != nil || !preserved {
				t.Fatal("earlier reason replaced", preserved, err)
			}
		})
	}
}

func TestSlackDeliveryAuditReadWithholding(t *testing.T) {
	f := newStatusFixture(t)
	f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "sessions.answer")
	f.outputWriter(t)("private fallback text")
	f.projectAll(t)
	post := f.firstPost(t)
	claim := f.postClaim(t, post)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	if err := FinishPost(t.Context(), f.Pool, policy, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	principal := f.Principal(t)

	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_delivery_read CHECK(action<>'session.content_read') NOT VALID`)
	rows, err := ListSessionDelivery(t.Context(), policy, f.Pool, principal, f.Environment, f.Session, 0, 100)
	if !errors.Is(err, audit.ErrCapture) || rows != nil {
		t.Fatalf("unaudited fallback released: %d %v", len(rows), err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE audit_events DROP CONSTRAINT reject_delivery_read`)
}

func TestSlackTerminalStreamExpirySettlesCleanupWithoutClaimingRemoteClosure(t *testing.T) {
	for _, cleanup := range []string{"open", "pending", "sending"} {
		t.Run(cleanup, func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.streamPost(t, 1, "retained stream")
			start := f.postClaim(t, post)
			policy := auditpolicy.Policy{RetentionSeconds: 3600}
			if err := FinishPost(t.Context(), f.Pool, policy, start, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			if err := f.finishUnknown(t, post); err != nil {
				t.Fatal(err)
			}
			observed := observedStream(t, f, post, "retained stream", "in_progress")
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observed); err != nil || !ok {
				t.Fatal(ok, err)
			}
			f.due(t)
			repair := f.claim(t, "active")
			if err := FinishStatus(t.Context(), f.Pool, policy, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			var stop PostClaim
			if cleanup != "open" {
				f.due(t)
				stop = f.postClaim(t, post)
				if stop.Method != "chat.stopStream" {
					t.Fatal(stop.Method)
				}
				if cleanup == "pending" {
					if err := FinishPost(t.Context(), f.Pool, policy, stop, DeliveryResult{Disposition: RateLimited, RetryAfter: time.Minute}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := session.ControlSession(t.Context(), policy, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "expire"}); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1;
 UPDATE projects SET history_retention_seconds=1 WHERE id=(SELECT project_id FROM environments WHERE id=(SELECT environment_id FROM sessions WHERE id=$1));
 UPDATE sessions SET history_eligible_at=clock_timestamp()-interval '2 seconds' WHERE id=$1;
 UPDATE slack_thread_sources p SET projected_event_seq=s.next_event_seq-1 FROM sessions s WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND s.id=$1`, pgx.QueryExecModeSimpleProtocol, f.Session)
			if err := RetainSessionHistory(t.Context(), f.Pool, f.Environment, f.Session); err != nil {
				t.Fatal(err)
			}
			if cleanup == "sending" {
				var retained bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT payload IS NOT NULL AND inflight_attempt_id=$2 FROM slack_posts WHERE id=$1`, post, stop.AttemptID).Scan(&retained); err != nil || !retained {
					t.Fatal("live cleanup claim was erased", retained, err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, post)
				if err := RetainSessionHistory(t.Context(), f.Pool, f.Environment, f.Session); err != nil {
					t.Fatal(err)
				}
			}
			var expired bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND payload IS NULL AND payload_expired_at IS NOT NULL AND stream_status='open' AND confirmed_stream_text='' AND inflight_attempt_id IS NULL AND claimed_until IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&expired); err != nil || !expired {
				t.Fatal("expiry lost unknown remote state or retained cleanup", expired, err)
			}
			f.noPostClaim(t, post)
			candidates, err := deliveryCandidates(t.Context(), f.Pool, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range candidates {
				if candidate.id == post {
					t.Fatal("expired stream rediscovered")
				}
			}
			if cleanup == "sending" {
				if err := FinishPost(t.Context(), f.Pool, policy, stop, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
				var unchanged bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT stream_status='open' AND payload IS NULL AND inflight_attempt_id IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&unchanged); err != nil || !unchanged {
					t.Fatal("stale cleanup resurrected expired record", unchanged, err)
				}
			}
		})
	}
}
