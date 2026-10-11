package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (f statusFixture) deliveryManager(t *testing.T) uuid.UUID {
	t.Helper()
	var org uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `UPDATE org_members SET role='owner' WHERE user_id=$1 RETURNING org_id`, f.User).Scan(&org); err != nil {
		t.Fatal(err)
	}
	return org
}
func (f statusFixture) recoveryInput(t *testing.T, org, post uuid.UUID) DeliveryRecovery {
	t.Helper()
	rows, err := ListSessionDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == post && row.AttemptID != nil {
			return DeliveryRecovery{AttemptID: *row.AttemptID, PublicationRevision: row.PublicationRevision}
		}
	}
	t.Fatal("uncertain attempt not visible")
	return DeliveryRecovery{}
}
func TestSlackDeliveryAbandonmentClosesPublicationWithoutReplacingRootOrReplaying(t *testing.T) {
	for _, unbound := range []bool{false, true} {
		t.Run(fmt.Sprintf("unbound=%v", unbound), func(t *testing.T) {
			f := newStatusFixture(t)
			org := f.deliveryManager(t)
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
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
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
			f.noPostClaim(t, response)
			input := f.recoveryInput(t, org, first)
			for range 2 {
				if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, first, input, "abandon"); err != nil {
					t.Fatal(err)
				}
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=$3 AND bool_and(delivery_disposed_at IS NOT NULL AND delivery_disposed_by=$2 AND closed_at IS NOT NULL AND suppressed_revision=desired_revision AND status IN ('failed','suppressed')) FROM slack_posts WHERE thread_source_id=$1 AND role=$4`, f.participant, f.User, map[bool]int{false: 2, true: 1}[unbound], map[bool]string{false: "intermediate", true: "opening"}[unbound]).Scan(&exact); err != nil || !exact {
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
				if err := f.Pool.QueryRow(t.Context(), `SELECT opening_publication_key=p.publication_key AND thread_ts IS NULL FROM slack_threads t JOIN slack_posts p ON p.id=$2 WHERE t.id=$1`, f.thread, first).Scan(&exact); err != nil || !exact {
					t.Fatal("abandoned opener replaced", exact, err)
				}
			} else {
				c := f.postClaim(t, response)
				if err := FinishPost(t.Context(), f.Pool, c, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !ok {
				t.Fatal("late positive evidence", ok, err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='delivery_abandoned' AND delivery_disposed_at IS NOT NULL AND confirmed_revision=1 AND message_ts='123.789' AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, first).Scan(&exact); err != nil || !exact {
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

func TestSlackDeliveryRecoveryRequiresCurrentManagerAndObservedPublication(t *testing.T) {
	f := newStatusFixture(t)
	org := f.deliveryManager(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	input := f.recoveryInput(t, org, post)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='developer' WHERE user_id=$1`, f.User)
	if _, err := ListSessionDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, 0, 100); err != nil {
		t.Fatal("reader denied", err)
	}
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); !errors.Is(err, errInstallationAuthority) {
		t.Fatal("developer changed delivery", err)
	}
	_ = f.deliveryManager(t)
	wrong := input
	wrong.AttemptID = uuid.NewV7()
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, wrong, "abandon"); !errors.Is(err, ErrDeliveryChanged) {
		t.Fatal("stale attempt", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET desired_revision=2 WHERE id=$1`, post)
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); !errors.Is(err, ErrDeliveryChanged) {
		t.Fatal("unseen content disposed", err)
	}
	input = f.recoveryInput(t, org, post)
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, uuid.NewV7(), post, input, "abandon"); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatal("foreign session", err)
	}
	if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); !errors.Is(err, ErrDeliveryChanged) {
		t.Fatal("confirmation lost to stale action", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.User)
	if _, err := ListSessionDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, 0, 100); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatal("disabled member read delivery", err)
	}
}

func TestSlackDeliveryHistoryChecksPauseAndExplicitlyResumeWithoutSending(t *testing.T) {
	for _, mode := range []string{"empty", "unavailable", "pagination", "page-budget"} {
		t.Run(mode, func(t *testing.T) {
			f := newStatusFixture(t)
			org := f.deliveryManager(t)
			post := f.post(t, 1, "content", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
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
			if mode == "page-budget" {
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
				t.Fatal("paused scan repeated", did, calls, err)
			}
			candidates, err := deliveryCandidates(t.Context(), f.Pool, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range candidates {
				if c.id == post {
					t.Fatal("paused post monopolizes discovery")
				}
			}
			var safe bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='uncertain' AND inflight_payload=$2 AND reconciliation_paused_at IS NOT NULL FROM slack_posts WHERE id=$1`, post, claim.Payload).Scan(&safe); err != nil || !safe {
				t.Fatal("negative evidence changed outcome", safe, err)
			}
			if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, f.recoveryInput(t, org, post), "check"); err != nil {
				t.Fatal(err)
			}
			f.historyDue(t, post)
			if did, err := ReconcileUnknownPost(t.Context(), f.Pool, client, post); err != nil || !did || calls != want+1 {
				t.Fatal("explicit check failed", did, calls, err)
			}
			f.due(t)
			f.noPostClaim(t, post)
		})
	}
}

func TestSlackDeliveryAbandonedStreamKeepsProofUntilPositiveEvidenceAndStopsWithoutContent(t *testing.T) {
	for _, method := range []string{"start", "append"} {
		t.Run(method, func(t *testing.T) {
			f := newStatusFixture(t)
			org := f.deliveryManager(t)
			post := f.streamPost(t, 1, "prefix")
			claim := f.postClaim(t, post)
			text := "prefix"
			if method == "append" {
				if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
					t.Fatal(err)
				}
				f.due(t)
				repair := f.claim(t, "active")
				if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
				text = "prefix appended"
				f.reviseStream(t, post, text)
				f.due(t)
				claim = f.postClaim(t, post)
			}
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			f.reviseStream(t, post, text+" NEVER_PUBLISH")
			if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, f.recoveryInput(t, org, post), "abandon"); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			f.noPostClaim(t, post)
			var safe bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND stream_state='uncertain' AND inflight_payload=$2 AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1`, post, claim.Payload).Scan(&safe); err != nil || !safe {
				t.Fatal("stream state or proof fabricated", safe, err)
			}
			m := observedStream(t, f, post, text, "in_progress")
			if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, m); err != nil || !ok {
				t.Fatal("late stream proof", ok, err)
			}
			f.due(t)
			repair := f.claim(t, "active")
			if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			f.due(t)
			stop := f.postClaim(t, post)
			if stop.Method != "chat.stopStream" || bytes.Contains(stop.Payload, []byte("chunks")) || bytes.Contains(stop.Payload, []byte("NEVER_PUBLISH")) {
				t.Fatal("abandoned bytes or wrong cleanup", stop.Method, string(stop.Payload))
			}
			if err := FinishPost(t.Context(), f.Pool, stop, DeliveryResult{Disposition: Acknowledged}); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='failed' AND error='delivery_abandoned' AND stream_state='stopped' AND delivery_disposed_at IS NOT NULL AND inflight_payload IS NULL FROM slack_posts WHERE id=$1`, post).Scan(&safe); err != nil || !safe {
				t.Fatal("cleanup reopened disposition", safe, err)
			}
			views, err := ListSessionDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(views)
			if bytes.Contains(raw, []byte("inflight_payload")) || bytes.Contains(raw, []byte("recipient_user_id")) {
				t.Fatal("transport details exposed")
			}
		})
	}
}

func TestSlackDeliveryAbandonmentWaitsForAcceptedProgressProjection(t *testing.T) {
	f := newStatusFixture(t)
	org := f.deliveryManager(t)
	write := f.outputWriter(t)
	write("first progress")
	f.projectAll(t)
	post := f.firstPost(t)
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	input := f.recoveryInput(t, org, post)
	write(" accepted before abandonment")
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); !errors.Is(err, ErrDeliveryChanged) {
		t.Fatalf("unprojected progress escaped observation: %v", err)
	}
	f.projectAll(t)
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); !errors.Is(err, ErrDeliveryChanged) {
		t.Fatalf("stale publication observation: %v", err)
	}
	input = f.recoveryInput(t, org, post)
	if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); err != nil {
		t.Fatal(err)
	}
	write("new progress after abandonment")
	f.projectAll(t)
	var raw []byte
	var next uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,payload FROM slack_posts WHERE thread_source_id=$1 AND id<>$2`, f.participant, post).Scan(&next, &raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("new progress after abandonment")) || bytes.Contains(raw, []byte("first progress")) || bytes.Contains(raw, []byte("accepted before abandonment")) {
		t.Fatalf("abandoned source replayed: %s", raw)
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
	if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet {
			t.Error("unknown delivery replayed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[]}`))}, nil
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
	if err := FinishPost(t.Context(), f.Pool, second, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.historyDue(t, post)
	var fresh bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT reconciliation_paused_at IS NULL AND reconciliation_pages=0 AND reconciliation_cursor='' FROM slack_posts WHERE id=$1`, post).Scan(&fresh); err != nil || !fresh {
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

func TestSlackDeliveryAbandonmentLeavesUnprojectedQuestionsAndResponseIndependent(t *testing.T) {
	for _, kind := range []string{"question", "answered", "response"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			org := f.deliveryManager(t)
			write := f.outputWriter(t)
			write("abandoned progress")
			f.projectAll(t)
			post := f.firstPost(t)
			first := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT turn_id FROM slack_posts WHERE id=$1`, post).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			role := "question"
			if kind == "response" {
				role = "response"
				if err := agent.RuntimeRespond(t.Context(), f.Pool, host, execution, turn, uuid.NewV7(), json.RawMessage(`"independent response"`)); err != nil {
					t.Fatal(err)
				}
				if err := agent.RuntimeCloseProcessing(t.Context(), f.Pool, host, execution, turn); err != nil {
					t.Fatal(err)
				}
				if _, err := agent.RuntimeFinalize(t.Context(), f.Pool, host, execution, turn, json.RawMessage(`null`), "callbacks drained"); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_saves s SET status='published',flush_acknowledged_at=statement_timestamp(),captured_at=statement_timestamp(),captured_root_digest=c.initial_root_digest,root_id=c.initial_root_id,capture_evidence='fixture cut',publication_evidence='fixture publication' FROM computers c WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.turn_id=$1`, turn)
				if err := agent.Complete(t.Context(), f.Pool, f.Environment, f.Session, turn); err != nil {
					t.Fatal(err)
				}
			} else {
				ask := uuid.NewV7()
				if err := agent.RuntimeAsk(t.Context(), f.Pool, host, execution, turn, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Independent question"}],"answer":{"type":"text"}}`)); err != nil {
					t.Fatal(err)
				}
				if kind == "answered" {
					if _, err := agent.RespondAsk(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, ResponseID: "answer", Answer: json.RawMessage(`"answer"`)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			input := f.recoveryInput(t, org, post)
			if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, input, "abandon"); err != nil {
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
	if err := FinishPost(t.Context(), f.Pool, delivery, DeliveryResult{Disposition: Uncertain}); err != nil {
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT reconciliation_paused_at IS NULL AND reconciliation_pages=1 AND next_attempt_at>clock_timestamp() AND inflight_payload=$2 FROM slack_posts WHERE id=$1`, post, delivery.Payload).Scan(&pending); err != nil || !pending {
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
	if err := FinishPost(t.Context(), f.Pool, delivery, DeliveryResult{Disposition: Uncertain}); err != nil {
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

func TestSlackDeliveryAbandonmentPreservesEarlierFailureReasons(t *testing.T) {
	for _, state := range []string{"failed", "suppressed"} {
		t.Run(state, func(t *testing.T) {
			f := newStatusFixture(t)
			org := f.deliveryManager(t)
			write := f.outputWriter(t)
			write(strings.Repeat("source", 700))
			f.projectAll(t)
			post := f.firstPost(t)
			claim := f.postClaim(t, post)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET status=$3,error='stream_content_rejected:invalid_blocks' WHERE thread_source_id=$1 AND id<>$2`, f.participant, post, state)
			if err := RecoverDelivery(t.Context(), f.Pool, org, f.User, f.Environment, f.Session, post, f.recoveryInput(t, org, post), "abandon"); err != nil {
				t.Fatal(err)
			}
			var preserved bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status=$3 AND error='stream_content_rejected:invalid_blocks' AND delivery_disposed_at IS NOT NULL FROM slack_posts WHERE thread_source_id=$1 AND id<>$2`, f.participant, post, state).Scan(&preserved); err != nil || !preserved {
				t.Fatal("earlier reason replaced", preserved, err)
			}
		})
	}
}
