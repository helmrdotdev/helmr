package slack

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

type statusFixture struct {
	sessiontest.Fixture
	installation, registration, publication, channel, thread, participant uuid.UUID
}

func newStatusFixture(t *testing.T) statusFixture {
	t.Helper()
	f := statusFixture{Fixture: sessiontest.New(t), installation: uuid.NewV7(), registration: uuid.NewV7(), publication: uuid.NewV7(), channel: uuid.NewV7(), thread: uuid.NewV7(), participant: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT 'http',$10,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$3 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$10,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$3 FROM environments WHERE id=$1;
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($5,$1,$6,$10,$2,$3,(SELECT org_id FROM slack_app_registrations WHERE id=$10));
 INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $4,$1,$5,$2,org_id,'team','C1' FROM environments WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key) SELECT $8,$1,$7,$4,org_id,'team','C1','123.456','opening' FROM environments WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($9,$8,$1,$7)`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.installation, f.User, f.channel, f.publication, f.Agent, f.Session, f.thread, f.participant, f.registration)

	sessiontest.ObserveProvisioning(t, f.Fixture)
	return f
}

func (f statusFixture) due(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH due AS (UPDATE slack_delivery_limits SET next_post_at=clock_timestamp(),next_status_at=clock_timestamp() WHERE installation_id=$1) UPDATE slack_method_limits SET retry_after=clock_timestamp() WHERE installation_id=$1;
 UPDATE slack_threads SET next_attempt_at=clock_timestamp(),refresh_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.installation, f.thread)
}

func (f statusFixture) claim(t *testing.T, want string) StatusClaim {
	t.Helper()
	claim, err := ClaimStatus(t.Context(), f.Pool, f.thread)
	if err != nil || claim == nil {
		t.Fatalf("claim %q: %+v %v", want, claim, err)
	}
	var payload map[string]string
	if err := json.Unmarshal(claim.Payload, &payload); err != nil || payload["status"] != want || payload["channel_id"] != "C1" || payload["thread_ts"] != "123.456" || len(payload) != 3 {
		t.Fatalf("payload %s: %v", claim.Payload, err)
	}
	return *claim
}

func TestSlackStatusAggregatesWorkBeforeAttentionAndRefreshesIdle(t *testing.T) {
	f := newStatusFixture(t)
	active := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, active, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	queued, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "work", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	f.due(t)
	working := f.claim(t, "processing")
	if working.Status == active.Status {
		t.Fatal("work did not revise aggregate")
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, working, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	peer, participant, hold := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth,origin_user_id,origin_api_key_id,origin_activation_id) SELECT environment_id,$2,agent_id,deployment_id,computer_id,root_session_id,id,id,1,origin_user_id,origin_api_key_id,origin_activation_id FROM sessions WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$5,$2);
 INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($5,$6,$2,'process_failed','system')`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, f.thread, f.Environment, hold)
	f.due(t)
	suspended := f.claim(t, "processing")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, suspended, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp() WHERE id=$1`, queued.TurnID)
	f.due(t)
	still := f.claim(t, "suspended")
	if still.Status == suspended.Status {
		t.Fatal("ending active work did not reveal held attention")
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, still, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE id=$1`, hold)
	f.due(t)
	idle := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, idle, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	refreshed := f.claim(t, "active")
	if refreshed.AttemptID == idle.AttemptID || refreshed.Status != idle.Status {
		t.Fatal("idle refresh stopped or invented core progress")
	}
}

func TestSlackStatusAmbiguityAndStaleCompletionCannotClearNewWork(t *testing.T) {
	f := newStatusFixture(t)
	old := f.claim(t, "active")
	if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
		t.Fatalf("overlapping claim: %+v %v", claim, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1;
 INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($2,$3,$4,'process_failed','system')`, pgx.QueryExecModeSimpleProtocol, f.thread, f.Environment, uuid.NewV7(), f.Session)
	f.due(t)
	current := f.claim(t, "processing")
	if allowed, err := statusClaimAuthorized(t.Context(), f.Pool, old); err != nil || allowed {
		t.Fatalf("superseded attempt authorized: %v %v", allowed, err)
	}
	if allowed, err := statusClaimAuthorized(t.Context(), f.Pool, current); err != nil || !allowed {
		t.Fatalf("current attempt denied: %v %v", allowed, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, old, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_attempt_id=$2 AND last_acknowledged_status IS NULL AND status_confirmation='unknown' FROM slack_threads WHERE id=$1`, f.thread, current.AttemptID).Scan(&exact); err != nil || !exact {
		t.Fatalf("stale finish changed lane: %v %v", exact, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, current, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_attempt_id IS NULL AND status_confirmation='unknown' FROM slack_threads WHERE id=$1`, f.thread).Scan(&exact); err != nil || !exact {
		t.Fatalf("ambiguity retained lane: %v %v", exact, err)
	}
	f.due(t)
	repair := f.claim(t, "processing")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	// No new core event is necessary to repair a delayed older remote effect.
	f.due(t)
	refresh := f.claim(t, "processing")
	if refresh.Status != repair.Status {
		t.Fatal("refresh depended on a new event")
	}
}

func TestSlackStatusRejectsRevokedRouteAndScopesAuthorizationLoss(t *testing.T) {
	for _, reauthorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-authorization", true: "new-authorization"}[reauthorized], func(t *testing.T) {
			f := newStatusFixture(t)
			claim := f.claim(t, "active")
			if reauthorized {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=authorized_at+interval '1 second' WHERE id=$1`, f.installation)
			}
			if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Rejected, Code: "token_revoked"}); err != nil {
				t.Fatal(err)
			}
			var lost bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT authorization_lost_at IS NOT NULL FROM slack_installations WHERE id=$1`, f.installation).Scan(&lost); err != nil || lost == reauthorized {
				t.Fatalf("wrong authorization generation disabled: %v %v", lost, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			f.due(t)
			if next, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || next != nil {
				t.Fatalf("revoked route claimed: %+v %v", next, err)
			}
		})
	}
}

func TestSlackStatusTerminalChildDoesNotMaskWorkingFront(t *testing.T) {
	f := newStatusFixture(t)
	peer, participant := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth,origin_user_id,origin_api_key_id,origin_activation_id) SELECT environment_id,$2,agent_id,deployment_id,computer_id,root_session_id,id,id,1,origin_user_id,origin_api_key_id,origin_activation_id FROM sessions WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$5,$2)`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, f.thread, f.Environment)
	caller := session.UserCaller(f.Principal(t))
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "work", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: peer, Kind: "interrupt", IdempotencyKey: "pause"}); err != nil {
		t.Fatal(err)
	}
	suspended := f.claim(t, "processing")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, suspended, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: peer, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.claim(t, "processing")
}

func TestSlackStatusRateLimitDelaysOtherThread(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t, "active")
	other, peer, participant := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id,origin_api_key_id,origin_activation_id) SELECT environment_id,$2,agent_id,deployment_id,computer_id,$2,0,origin_user_id,origin_api_key_id,origin_activation_id FROM sessions WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key) SELECT $4,$6,$2,$5,org_id,team_id,slack_channel_id,'789.012','other' FROM slack_channel_bindings WHERE id=$5;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$6,$2)`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, other, f.channel, f.Environment)
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: RateLimited, Code: "ratelimited", RetryAfter: 90 * time.Second}); err != nil {
		t.Fatal(err)
	}
	var remaining float64
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch from retry_after-clock_timestamp()) FROM slack_method_limits WHERE installation_id=$1 AND method='agents.sessions.setStatus'`, f.installation).Scan(&remaining); err != nil || remaining < 85 {
		t.Fatalf("workspace cooldown: %v %v", remaining, err)
	}
	if next, err := ClaimStatus(t.Context(), f.Pool, other); err != nil || next != nil {
		t.Fatalf("other thread ignored cooldown: %+v %v", next, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH due AS (UPDATE slack_delivery_limits SET next_post_at=clock_timestamp(),next_status_at=clock_timestamp() WHERE installation_id=$1) UPDATE slack_method_limits SET retry_after=clock_timestamp() WHERE installation_id=$1`, f.installation)
	if next, err := ClaimStatus(t.Context(), f.Pool, other); err != nil || next == nil {
		t.Fatalf("other thread not released: %+v %v", next, err)
	}
}

func TestSlackStatusDisabledFeatureDoesNotRevokeInstallation(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Rejected, Code: "feature_disabled"}); err != nil {
		t.Fatal(err)
	}
	var usable bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.authorization_lost_at IS NULL AND t.delivery_error='feature_disabled' AND t.status_confirmation='unknown' FROM slack_installations i JOIN slack_channel_bindings c ON c.installation_id=i.id JOIN slack_threads t ON t.slack_channel_binding_id=c.id WHERE t.id=$1`, f.thread).Scan(&usable); err != nil || !usable {
		t.Fatalf("feature failure revoked installation or lost diagnostic: %v %v", usable, err)
	}
	f.due(t)
	f.claim(t, "active")
}

func TestSlackStatusAcknowledgesOnlyTheClaimedValue(t *testing.T) {
	f := newStatusFixture(t)
	first := f.claim(t, "active")
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "while-sending", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	// Recompute the desired state while the original HTTP attempt still owns the lane.
	if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
		t.Fatalf("overlapping claim: %+v %v", claim, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_acknowledged_status=$2 AND desired_status='processing' AND inflight_attempt_id IS NULL FROM slack_threads WHERE id=$1`, f.thread, first.Status).Scan(&exact); err != nil || !exact {
		t.Fatalf("old success acknowledged newer work: %v %v", exact, err)
	}
	f.due(t)
	next := f.claim(t, "processing")
	if next.AttemptID == first.AttemptID || next.Status == first.Status {
		t.Fatal("new state did not get a new attempt", next)
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_acknowledged_status=$2 AND inflight_attempt_id=$3 FROM slack_threads WHERE id=$1`, f.thread, first.Status, next.AttemptID).Scan(&exact); err != nil || !exact {
		t.Fatalf("duplicate success changed newer attempt: %v %v", exact, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, next, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_acknowledged_status=desired_status AND last_acknowledged_status=$2 FROM slack_threads WHERE id=$1`, f.thread, next.Status).Scan(&exact); err != nil || !exact {
		t.Fatalf("latest state not acknowledged: %v %v", exact, err)
	}
}

func TestSlackStatusCoalescesReturnToAcknowledgedValue(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-send", true: "during-send"}[inFlight], func(t *testing.T) {
			f := newStatusFixture(t)
			first := f.claim(t, "active")
			if !inFlight {
				if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
				// Defer sending without deferring observation of authoritative state.
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, f.thread)
			}
			turn, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "brief-work", Input: json.RawMessage(`[]`)})
			if err != nil {
				t.Fatal(err)
			}
			if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
				t.Fatalf("deferred processing claim: %+v %v", claim, err)
			}
			var desired string
			if err := f.Pool.QueryRow(t.Context(), `SELECT desired_status FROM slack_threads WHERE id=$1`, f.thread).Scan(&desired); err != nil || desired != "processing" {
				t.Fatalf("intermediate state not observed: %s %v", desired, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp() WHERE id=$1`, turn.TurnID)
			if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
				t.Fatalf("deferred active claim: %+v %v", claim, err)
			}
			if inFlight {
				if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, first, DeliveryResult{Disposition: Acknowledged}); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_delivery_limits SET next_status_at=clock_timestamp() WHERE installation_id=$1;
 UPDATE slack_threads SET next_attempt_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.installation, f.thread)
			if claim, err := ClaimStatus(t.Context(), f.Pool, f.thread); err != nil || claim != nil {
				t.Fatalf("equal acknowledged state resent: %+v %v", claim, err)
			}
			// The equal state no longer takes the dirty-status lead over waiting content.
			post := f.post(t, 1, "waiting-content", "lifecycle", nil)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET created_at=(SELECT refresh_at-interval '15 seconds' FROM slack_threads WHERE id=$2) WHERE id=$1`, post, f.thread)
			candidates, err := deliveryCandidates(t.Context(), f.Pool, 4)
			if err != nil || len(candidates) != 2 || candidates[0].id != post || candidates[1].id != f.thread {
				t.Fatalf("coalesced status displaced content or lost discovery: %+v %v", candidates, err)
			}
			f.due(t)
			f.claim(t, "active")
		})
	}
}

func TestSlackStatusAcceptsExpiredUnreplacedCompletion(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t, "active")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.thread)
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var acknowledged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_acknowledged_status='active' AND status_confirmation='acknowledged' AND inflight_attempt_id IS NULL FROM slack_threads WHERE id=$1`, f.thread).Scan(&acknowledged); err != nil || !acknowledged {
		t.Fatalf("unreplaced completion discarded: %v %v", acknowledged, err)
	}
}

func TestSlackStatusPeriodicAssertionRepairsLateRemoteWrite(t *testing.T) {
	f := newStatusFixture(t)
	old := f.claim(t, "active")
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "current-work", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	// Recover after the original sender disappeared with an unresolved request.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.thread)
	f.due(t)
	remote := ""
	calls := 0
	client := callerFunc(func(_ context.Context, _ uuid.UUID, _ int64, method string, payload []byte) DeliveryResult {
		var request map[string]string
		if err := json.Unmarshal(payload, &request); err != nil || method != "agents.sessions.setStatus" {
			t.Fatalf("status request %s %s: %v", method, payload, err)
		}
		remote = request["status"]
		calls++
		return DeliveryResult{Disposition: Acknowledged}
	})
	if sent, err := ReconcileStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, client, f.thread); err != nil || !sent || remote != "processing" {
		t.Fatalf("recovery assertion: %v %s %v", sent, remote, err)
	}
	// Slack may apply an obsolete request even after its local owner was replaced.
	remote = old.Status
	if err := FinishStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, old, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var current bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_acknowledged_status='processing' AND desired_status='processing' AND status_confirmation='acknowledged' FROM slack_threads WHERE id=$1`, f.thread).Scan(&current); err != nil || !current {
		t.Fatalf("obsolete response changed local confirmation: %v %v", current, err)
	}
	f.due(t)
	if sent, err := ReconcileStatus(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, client, f.thread); err != nil || !sent || remote != "processing" || calls != 2 {
		t.Fatalf("same-value refresh failed to repair remote: %v %s calls=%d %v", sent, remote, calls, err)
	}
}
