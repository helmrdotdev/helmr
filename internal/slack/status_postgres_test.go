package slack

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

type statusFixture struct {
	agenttest.Fixture
	installation, registration, publication, channel, thread, participant uuid.UUID
}

func newStatusFixture(t *testing.T) statusFixture {
	t.Helper()
	f := statusFixture{Fixture: agenttest.New(t), installation: uuid.NewV7(), registration: uuid.NewV7(), publication: uuid.NewV7(), channel: uuid.NewV7(), thread: uuid.NewV7(), participant: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT $10,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$3 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$10,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$3 FROM environments WHERE id=$1;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($5,$1,$6,$10,$2,$3);
 INSERT INTO slack_channels(id,environment_id,publication_id,installation_id,organization_id,team_id,slack_channel_id) SELECT $4,$1,$5,$2,org_id,'team','C1' FROM environments WHERE id=$1;
 UPDATE sessions SET slack_channel_id=$4 WHERE id=$7;
 INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key) SELECT $8,$1,$7,$4,org_id,'team','C1','123.456','opening' FROM environments WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($9,$8,$1,$7)`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.installation, f.User, f.channel, f.publication, f.Agent, f.Session, f.thread, f.participant, f.registration)

	return f
}

func (f statusFixture) due(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$1;
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

func TestSlackStatusAggregatesAttentionBeforeWorkAndRefreshesIdle(t *testing.T) {
	f := newStatusFixture(t)
	active := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, active, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	queued, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "work", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	f.due(t)
	working := f.claim(t, "processing")
	if working.Revision <= active.Revision {
		t.Fatal("work did not revise aggregate")
	}
	if err := FinishStatus(t.Context(), f.Pool, working, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	peer, participant, hold := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth)
 SELECT history_retention_mode,environment_id,$2,agent_id,deployment_id,computer_id,root_session_id,id,id,1 FROM sessions WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$5,$2);
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($5,$6,$2,'local','needs attention')`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, f.thread, f.Environment, hold)
	f.due(t)
	suspended := f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, suspended, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp() WHERE id=$1`, queued.TurnID)
	f.due(t)
	still := f.claim(t, "suspended")
	if still.Revision != suspended.Revision {
		t.Fatal("unchanged attention invented a state revision")
	}
	if err := FinishStatus(t.Context(), f.Pool, still, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE id=$1`, hold)
	f.due(t)
	idle := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, idle, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	refreshed := f.claim(t, "active")
	if refreshed.Epoch <= idle.Epoch || refreshed.Revision != idle.Revision {
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
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($2,$3,$4,'local','attention')`, pgx.QueryExecModeSimpleProtocol, f.thread, f.Environment, uuid.NewV7(), f.Session)
	f.due(t)
	current := f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, old, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_attempt_id=$2 AND confirmed_revision=0 AND status_confirmation='unknown' FROM slack_threads WHERE id=$1`, f.thread, current.AttemptID).Scan(&exact); err != nil || !exact {
		t.Fatalf("stale finish changed lane: %v %v", exact, err)
	}
	if err := FinishStatus(t.Context(), f.Pool, current, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT inflight_attempt_id IS NULL AND status_confirmation='unknown' FROM slack_threads WHERE id=$1`, f.thread).Scan(&exact); err != nil || !exact {
		t.Fatalf("ambiguity retained lane: %v %v", exact, err)
	}
	f.due(t)
	repair := f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, repair, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	// No new core event is necessary to repair a delayed older remote effect.
	f.due(t)
	refresh := f.claim(t, "suspended")
	if refresh.Revision != repair.Revision {
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
			if err := FinishStatus(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Rejected, Code: "token_revoked"}); err != nil {
				t.Fatal(err)
			}
			var lost bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT authorization_lost_at IS NOT NULL FROM slack_installations WHERE id=$1`, f.installation).Scan(&lost); err != nil || lost == reauthorized {
				t.Fatalf("wrong authorization generation disabled: %v %v", lost, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
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
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth)
 SELECT history_retention_mode,environment_id,$2,agent_id,deployment_id,computer_id,root_session_id,id,id,1 FROM sessions WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$5,$2)`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, f.thread, f.Environment)
	caller := agent.Caller{Kind: "user", ID: f.User}
	if _, err := agent.Enqueue(t.Context(), f.Pool, caller, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "work", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, caller, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: peer, Kind: "interrupt", RetryKey: "pause", Reason: "attention"}); err != nil {
		t.Fatal(err)
	}
	suspended := f.claim(t, "suspended")
	if err := FinishStatus(t.Context(), f.Pool, suspended, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, caller, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: peer, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	f.claim(t, "processing")
}

func TestSlackStatusRateLimitDelaysOtherThread(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t, "active")
	other, peer, participant := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,slack_channel_id,causal_depth)
 SELECT history_retention_mode,environment_id,$2,agent_id,deployment_id,computer_id,$2,slack_channel_id,0 FROM sessions WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key) SELECT $4,$6,$2,$5,organization_id,team_id,slack_channel_id,'789.012','other' FROM slack_channels WHERE id=$5;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($3,$4,$6,$2)`, pgx.QueryExecModeSimpleProtocol, f.Session, peer, participant, other, f.channel, f.Environment)
	if err := FinishStatus(t.Context(), f.Pool, claim, DeliveryResult{Disposition: RateLimited, Code: "ratelimited", RetryAfter: 90 * time.Second}); err != nil {
		t.Fatal(err)
	}
	var remaining float64
	if err := f.Pool.QueryRow(t.Context(), `SELECT extract(epoch from delivery_next_at-clock_timestamp()) FROM slack_installations WHERE id=$1`, f.installation).Scan(&remaining); err != nil || remaining < 85 {
		t.Fatalf("workspace cooldown: %v %v", remaining, err)
	}
	if next, err := ClaimStatus(t.Context(), f.Pool, other); err != nil || next != nil {
		t.Fatalf("other thread ignored cooldown: %+v %v", next, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$1`, f.installation)
	if next, err := ClaimStatus(t.Context(), f.Pool, other); err != nil || next == nil {
		t.Fatalf("other thread not released: %+v %v", next, err)
	}
}

func TestSlackStatusDisabledFeatureDoesNotRevokeInstallation(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t, "active")
	if err := FinishStatus(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Rejected, Code: "feature_disabled"}); err != nil {
		t.Fatal(err)
	}
	var usable bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.authorization_lost_at IS NULL AND t.delivery_error='feature_disabled' AND t.status_confirmation='unknown' FROM slack_installations i JOIN slack_channels c ON c.installation_id=i.id JOIN slack_threads t ON t.channel_id=c.id WHERE t.id=$1`, f.thread).Scan(&usable); err != nil || !usable {
		t.Fatalf("feature failure revoked installation or lost diagnostic: %v %v", usable, err)
	}
	f.due(t)
	f.claim(t, "active")
}
