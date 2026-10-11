package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func deliveryTestClient(handler func(*http.Request) string) *WebClient {
	return NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/conversations.info" {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":{"id":"C1","name":"work","context_team_id":"team","is_channel":true,"is_member":true}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(handler(r)))}, nil
	}))
}

func TestSlackDeliveryWorkerRotatesBlockedRootToIndependentThread(t *testing.T) {
	f := newStatusFixture(t)
	blocked := f.post(t, 1, "later", "lifecycle", nil)
	peer := f
	peer.Session, peer.thread, peer.participant = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1;
 INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,slack_channel_id,causal_depth)
 SELECT history_retention_mode,environment_id,$3,agent_id,deployment_id,computer_id,$3,slack_channel_id,0 FROM sessions WHERE id=$2;
 INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key) SELECT $4,$7,$3,$5,organization_id,team_id,slack_channel_id,'456.789','peer-opening' FROM slack_channels WHERE id=$5;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($6,$4,$7,$3);
 UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`, pgx.QueryExecModeSimpleProtocol, f.thread, f.Session, peer.Session, peer.thread, f.channel, peer.participant, f.Environment)
	independent := peer.post(t, 1, "independent", "lifecycle", nil)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string { calls++; return `{"ok":true,"ts":"456.790","channel":"C1"}` })
	for range 2 {
		if n, err := ReconcileDelivery(t.Context(), f.Pool, client, 1); err != nil || n != 1 {
			t.Fatalf("scan: %d %v", n, err)
		}
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='pending' AND attempt_count=0 FROM slack_posts WHERE id=$1) AND (SELECT status='posted' FROM slack_posts WHERE id=$2)`, blocked, independent).Scan(&correct); err != nil || !correct || calls != 1 {
		t.Fatalf("blocked root monopolized scan: %v calls=%d %v", correct, calls, err)
	}
}

func TestSlackDeliveryWorkerRecoversExpiredMutationThroughReadOnlyEvidence(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1;
 UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`, pgx.QueryExecModeSimpleProtocol, post)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string {
		calls++
		if r.Method != http.MethodGet {
			t.Fatal("expired mutation was resent")
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{observedClaim(t, claim)}})
		return string(body)
	})
	if n, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND attempt_count=1 FROM slack_posts WHERE id=$1`, post).Scan(&correct); err != nil || !correct || calls != 1 {
		t.Fatalf("recovery evidence: %v calls=%d %v", correct, calls, err)
	}
}

func TestSlackDeliveryWorkersDoNotDuplicateMutation(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`)
	var calls atomic.Int32
	client := deliveryTestClient(func(r *http.Request) string { calls.Add(1); return `{"ok":true,"ts":"123.789","channel":"C1"}` })
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' AND attempt_count=1 FROM slack_posts WHERE id=$1`, post).Scan(&posted); err != nil || !posted || calls.Load() != 1 {
		t.Fatalf("duplicate delivery: %v %d %v", posted, calls.Load(), err)
	}
}

func TestSlackDeliveryWorkerRefreshesStatusAndPreservesRetryDelay(t *testing.T) {
	f := newStatusFixture(t)
	client := deliveryTestClient(func(r *http.Request) string { return `{"ok":true,"agent_status":"active"}` })
	if n, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil || n != 1 {
		t.Fatalf("status: %d %v", n, err)
	}
	var confirmed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status_confirmation='acknowledged' AND confirmed_revision=desired_revision FROM slack_threads WHERE id=$1`, f.thread).Scan(&confirmed); err != nil || !confirmed {
		t.Fatalf("status not confirmed: %v %v", confirmed, err)
	}
	post := f.post(t, 1, "content", "lifecycle", nil)
	f.due(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`)
	limited := deliveryTestClient(func(r *http.Request) string { return `{"ok":false,"error":"ratelimited"}` })
	if _, err := ReconcileDelivery(t.Context(), f.Pool, limited, 4); err != nil {
		t.Fatal(err)
	}
	var delayed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND next_attempt_at>clock_timestamp()+interval '25 seconds' FROM slack_posts WHERE id=$1`, post).Scan(&delayed); err != nil || !delayed {
		t.Fatalf("retry shortened: %v %v", delayed, err)
	}
}

func TestSlackDeliveryDiscoverySkipsLiveStatusClaimAndRecoversExpiredClaim(t *testing.T) {
	f := newStatusFixture(t)
	f.claim(t, "active")
	post := f.post(t, 1, "content", "lifecycle", nil)
	candidates, err := deliveryCandidates(t.Context(), f.Pool, 1)
	if err != nil || len(candidates) != 1 || candidates[0].id != post {
		t.Fatalf("live status blocked other discovery: %v %v", candidates, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.thread)
	candidates, err = deliveryCandidates(t.Context(), f.Pool, 1)
	if err != nil || len(candidates) != 1 || candidates[0].id != f.thread {
		t.Fatalf("expired status not recovered: %v %v", candidates, err)
	}
}

func TestSlackDeliveryWorkerSelectsQuestionAheadOfBlockedOutputBacklog(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	write("prefix")
	f.projectAll(t)
	first := f.firstPost(t)
	claim := f.postClaim(t, first)
	if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	// An ordinary authored output budget can create thousands of continuations.
	// Here all earlier rows depend on the unknown first mutation.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_posts(id,environment_id,session_id,thread_id,thread_source_id,seq,publication_key,continuation_ordinal,role,turn_id,payload,payload_digest,presentation_path)
 SELECT gen_random_uuid(),environment_id,session_id,thread_id,thread_source_id,seq+n,publication_key,n,role,turn_id,payload,payload_digest,presentation_path FROM slack_posts CROSS JOIN generate_series(1,1000) n WHERE id=$1;
 UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour';
 UPDATE slack_posts SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, first)
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	ask := uuid.NewV7()
	if err := agent.RuntimeAsk(t.Context(), f.Pool, host, execution, turn, ask, json.RawMessage(`{"prompt":[{"type":"text","text":"Proceed?"}],"answer":{"type":"text"}}`)); err != nil {
		t.Fatal(err)
	}
	f.projectAll(t)
	f.due(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string { calls++; return `{"ok":true,"ts":"123.791","channel":"C1"}` })
	if n, err := ReconcileDelivery(t.Context(), f.Pool, client, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var answered bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' FROM slack_posts WHERE ask_id=$1`, ask).Scan(&answered); err != nil || !answered || calls != 1 {
		t.Fatal("question waited behind output backlog", answered, calls, err)
	}
}

func TestSlackDeliveryWorkerFairBatchDoesNotStarveQuestionBehindIdleStatus(t *testing.T) {
	f := newStatusFixture(t)
	turn, ask := f.pendingQuestion(t)
	_ = turn
	f.projectAll(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH ids AS (SELECT gen_random_uuid() id,n FROM generate_series(1,80) n),
 added_sessions AS (INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,slack_channel_id,causal_depth)
 SELECT s.history_retention_mode,s.environment_id,i.id,s.agent_id,s.deployment_id,s.computer_id,i.id,s.slack_channel_id,0 FROM sessions s CROSS JOIN ids i WHERE s.id=$1 RETURNING id),
 added_threads AS (INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key)
 SELECT s.id,$3,s.id,$2,c.organization_id,c.team_id,c.slack_channel_id,'123.'||i.n::text,s.id::text FROM added_sessions s JOIN ids i ON i.id=s.id CROSS JOIN slack_channels c WHERE c.id=$2 RETURNING id)
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id)
 SELECT id,id,$3,id FROM added_threads;
 UPDATE slack_threads SET status_confirmation='acknowledged',desired_status='active',confirmed_revision=desired_revision,refresh_at=clock_timestamp()+interval '1 hour'`, pgx.QueryExecModeSimpleProtocol, f.Session, f.channel, f.Environment)
	// These idle status rows remain discoverable for core-state changes, but must
	// not occupy every slot ahead of answerable work.
	f.due(t)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string { calls++; return `{"ok":true,"ts":"123.791","channel":"C1"}` })
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' FROM slack_posts WHERE ask_id=$1`, ask).Scan(&posted); err != nil || !posted || calls < 1 {
		t.Fatal("idle status starved question", posted, calls, err)
	}
}

func TestSlackDeliveryWorkerExpiresIssuedReplyAfterRootDeletion(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-revision", true: "newer-desired"}[newer], func(t *testing.T) {
			f := newStatusFixture(t)
			post := f.post(t, 1, "reply", "lifecycle", nil)
			claim := f.postClaim(t, post)
			if newer {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET desired_revision=desired_revision+1 WHERE id=$1`, post)
			}
			h := EventHandler{Database: f.Pool, AppID: "app"}
			if err := h.observeDeletion(t.Context(), "team", "C1", "123.456"); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, post)
			client := deliveryTestClient(func(r *http.Request) string {
				if r.Method != http.MethodGet {
					t.Fatal("issued reply replayed after deletion")
				}
				body, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{observedClaim(t, claim)}})
				return string(body)
			})
			if n, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			var known bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status=$2 AND claimed_until IS NULL AND inflight_attempt_id IS NULL AND closed_at IS NOT NULL AND (NOT $3 OR (suppressed_revision=desired_revision AND confirmed_revision<desired_revision AND error IS NOT NULL)) FROM slack_posts WHERE id=$1`, post, map[bool]string{false: "posted", true: "suppressed"}[newer], newer).Scan(&known); err != nil || !known {
				t.Fatal("issued deleted-root claim stranded", err)
			}
		})
	}
}

func TestSlackDeliveryWorkerQuestionGetsBudgetBeforeUnrelatedRoots(t *testing.T) {
	f := newStatusFixture(t)
	// Earlier first publications share the same installation's send budget.
	for n := range 6 {
		peer := f
		peer.Session, peer.thread, peer.participant = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,slack_channel_id,causal_depth)
 SELECT history_retention_mode,environment_id,$2,agent_id,deployment_id,computer_id,$2,slack_channel_id,0 FROM sessions WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,opening_publication_key) SELECT $3,$6,$2,$4,organization_id,team_id,slack_channel_id,$2::uuid::text FROM slack_channels WHERE id=$4;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($5,$3,$6,$2)`, pgx.QueryExecModeSimpleProtocol, f.Session, peer.Session, peer.thread, f.channel, peer.participant, f.Environment)
		peer.post(t, int64(n+1), peer.Session.String(), "lifecycle", nil)
	}
	_, ask := f.pendingQuestion(t)
	f.projectAll(t)
	f.due(t)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["thread_ts"]) != `"123.456"` {
			t.Fatal("unrelated root consumed question budget", string(body["thread_ts"]))
		}
		return `{"ok":true,"ts":"123.791","channel":"C1"}`
	})
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' FROM slack_posts WHERE ask_id=$1`, ask).Scan(&posted); err != nil || !posted || calls != 1 {
		t.Fatal("question pacing", posted, calls, err)
	}
}

func TestSlackDeliveryWorkerStatusSuccessDoesNotEraseWaitingContentAge(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "waiting", "lifecycle", nil)
	// Let the first assertion precede content. Only successful service should
	// change that precedence; retry eligibility must not erase the waiting age.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET refresh_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.thread)
	var methods []string
	client := deliveryTestClient(func(r *http.Request) string {
		methods = append(methods, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "agents.sessions.setStatus") {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			result, _ := json.Marshal(map[string]any{"ok": true, "agent_status": body["status"]})
			return string(result)
		}
		return `{"ok":true,"ts":"123.789","channel":"C1"}`
	})
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || !strings.HasSuffix(methods[0], "agents.sessions.setStatus") {
		t.Fatal("initial assertion", methods)
	}
	// Change authoritative core state while retaining FinishStatus's refresh_at.
	if _, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "status-change", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$1;
 UPDATE slack_threads SET next_attempt_at=clock_timestamp() WHERE id=$2;
 UPDATE slack_posts SET next_attempt_at=clock_timestamp() WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.installation, f.thread, post)
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' FROM slack_posts WHERE id=$1`, post).Scan(&posted); err != nil || !posted || len(methods) != 2 || !strings.HasSuffix(methods[1], "chat.postMessage") {
		t.Fatal("changing status starved waiting content", posted, methods, err)
	}
}

func TestSlackDeliveryWorkerWaitingContentPrecedesPeriodicRefreshPopulation(t *testing.T) {
	f := newStatusFixture(t)
	post := f.post(t, 1, "waiting", "lifecycle", nil)
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH ids AS (SELECT gen_random_uuid() id,n FROM generate_series(1,12) n),
 added_sessions AS (INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,slack_channel_id,causal_depth)
 SELECT s.history_retention_mode,s.environment_id,i.id,s.agent_id,s.deployment_id,s.computer_id,i.id,s.slack_channel_id,0 FROM sessions s CROSS JOIN ids i WHERE s.id=$1 RETURNING id),
 added_threads AS (INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key)
 SELECT s.id,$3,s.id,$2,c.organization_id,c.team_id,c.slack_channel_id,'456.'||i.n::text,s.id::text FROM added_sessions s JOIN ids i ON i.id=s.id CROSS JOIN slack_channels c WHERE c.id=$2 RETURNING id)
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id)
 SELECT id,id,$3,id FROM added_threads;
 UPDATE slack_threads SET status_confirmation='acknowledged',desired_status='active',confirmed_revision=desired_revision,refresh_at=clock_timestamp()-interval '1 minute';
 UPDATE slack_posts SET created_at=clock_timestamp()-interval '2 minutes' WHERE id=$4`, pgx.QueryExecModeSimpleProtocol, f.Session, f.channel, f.Environment, post)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string {
		calls++
		if r.URL.Path != "/api/chat.postMessage" {
			t.Fatal("periodic refresh consumed content slot", r.URL.Path)
		}
		return `{"ok":true,"ts":"123.789","channel":"C1"}`
	})
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var posted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='posted' FROM slack_posts WHERE id=$1`, post).Scan(&posted); err != nil || !posted || calls != 1 {
		t.Fatal("periodic refresh starved content", posted, calls, err)
	}
}

func TestSlackDeliveryWorkerAgeOrderingPreservesStreamRepair(t *testing.T) {
	f := newStatusFixture(t)
	post := f.streamPost(t, 1, "visible")
	next := f.streamPost(t, 2, "later")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET closed_at=clock_timestamp() WHERE id IN ($1,$2);
 UPDATE slack_threads SET status_confirmation='acknowledged',confirmed_revision=desired_revision,refresh_at=clock_timestamp()+interval '1 hour' WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, post, next, f.thread)
	var methods []string
	client := deliveryTestClient(func(r *http.Request) string {
		methods = append(methods, r.URL.Path)
		if r.URL.Path == "/api/agents.sessions.setStatus" {
			return `{"ok":true,"agent_status":"active"}`
		}
		return `{"ok":true,"ts":"123.789","channel":"C1"}`
	})
	want := []string{"/api/chat.startStream", "/api/agents.sessions.setStatus", "/api/chat.stopStream", "/api/agents.sessions.setStatus", "/api/chat.startStream"}
	for i, method := range want {
		// Advance only retry eligibility. Keep publication age, repair obligations
		// and the refresh deadline written by the real completion paths unchanged.
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$1;
 UPDATE slack_posts SET next_attempt_at=clock_timestamp();
 UPDATE slack_threads SET next_attempt_at=clock_timestamp()`, pgx.QueryExecModeSimpleProtocol, f.installation)
		if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
			t.Fatal(err)
		}
		if len(methods) != i+1 || methods[i] != method {
			t.Fatalf("repair order step %d: %v", i, methods)
		}
	}
}

func TestSlackDeliveryWorkerUnconfirmedStatusPrecedesNewerContent(t *testing.T) {
	f := newStatusFixture(t)
	initial := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, initial, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "new-status", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	// Discover the core change while the previous call still owns the budget.
	if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
		t.Fatal("expected deferred assertion", claim, err)
	}
	post := f.post(t, 1, "newer-content", "lifecycle", nil)
	f.post(t, 2, "newer-content-2", "lifecycle", nil)
	f.post(t, 3, "newer-content-3", "lifecycle", nil)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$1`, f.installation)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string {
		calls++
		if r.URL.Path != "/api/agents.sessions.setStatus" {
			t.Fatal("newer content displaced known status change", r.URL.Path)
		}
		return `{"ok":true,"agent_status":"processing"}`
	})
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='pending' FROM slack_posts WHERE id=$1) AND (SELECT status_confirmation='acknowledged' AND desired_status='processing' AND confirmed_revision=desired_revision FROM slack_threads WHERE id=$2)`, post, f.thread).Scan(&exact); err != nil || !exact || calls != 1 {
		t.Fatal("changed status freshness", exact, calls, err)
	}
}

func TestSlackDeliveryWorkerBlockedContinuationsDoNotHideReadyStream(t *testing.T) {
	f := newStatusFixture(t)
	first := f.streamPost(t, 1, "first")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET closed_at=clock_timestamp() WHERE id=$1;
 INSERT INTO slack_posts(id,environment_id,session_id,thread_id,thread_source_id,seq,publication_key,continuation_ordinal,role,turn_id,payload,payload_digest,presentation_path,recipient_team_id,recipient_user_id,closed_at,next_attempt_at)
 SELECT gen_random_uuid(),environment_id,session_id,thread_id,thread_source_id,seq+n,publication_key,n,role,turn_id,payload,payload_digest,presentation_path,recipient_team_id,recipient_user_id,closed_at,clock_timestamp()-interval '1 hour' FROM slack_posts CROSS JOIN generate_series(1,17) n WHERE id=$1;
 UPDATE slack_threads SET status_confirmation='acknowledged',confirmed_revision=desired_revision,refresh_at=clock_timestamp()+interval '1 hour' WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, first, f.thread)
	calls := 0
	client := deliveryTestClient(func(r *http.Request) string {
		calls++
		if r.URL.Path != "/api/chat.startStream" {
			t.Fatal("unexpected mutation", r.URL.Path)
		}
		return `{"ok":true,"ts":"123.789","channel":"C1"}`
	})
	if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
		t.Fatal(err)
	}
	var started bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND stream_state='open' AND message_ts='123.789' AND confirmed_revision=desired_revision FROM slack_posts WHERE id=$1`, first).Scan(&started); err != nil || !started || calls != 1 {
		t.Fatal("blocked tails displaced ready stream", started, calls, err)
	}
}

func TestSlackDeliveryWorkerSuppressesBlockedTailWhenPublicationAuthorityEnds(t *testing.T) {
	for _, reason := range []string{"channel", "speaker", "disconnected", "authorization"} {
		t.Run(reason, func(t *testing.T) {
			f := newStatusFixture(t)
			first := f.post(t, 1, "chain", "lifecycle", nil)
			claim := f.postClaim(t, first)
			if err := FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
				t.Fatal(err)
			}
			tail := f.post(t, 2, "tail", "lifecycle", nil)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET reconciliation_paused_at=clock_timestamp() WHERE id=$1;
 UPDATE slack_posts SET publication_key='chain',continuation_ordinal=1 WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, first, tail)
			switch reason {
			case "channel":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "speaker":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "disconnected":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET disconnected_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, f.installation)
			case "authorization":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			}
			client := deliveryTestClient(func(r *http.Request) string {
				t.Fatal("unavailable authority made HTTP request", r.URL.Path)
				return ""
			})
			if _, err := ReconcileDelivery(t.Context(), f.Pool, client, 4); err != nil {
				t.Fatal(err)
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='uncertain' AND reconciliation_paused_at IS NOT NULL FROM slack_posts WHERE id=$1) AND (SELECT status='suppressed' AND error='publication_authority_unavailable' FROM slack_posts WHERE id=$2)`, first, tail).Scan(&exact); err != nil || !exact {
				t.Fatal("blocked tail cleanup hidden", exact, err)
			}
		})
	}
}
