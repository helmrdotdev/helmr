package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestSlackSharedConversationRequiresUseWithoutManagement(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	ctx := t.Context()
	f.link(t)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE roles SET permissions=ARRAY['agents.use']`)
	root := slackNow()
	first := f.newMessage(t, "shared-start", root)
	if err := admitMessage(ctx, f.Pool, nil, f.admissionClient(t), first); err != nil {
		t.Fatal(err)
	}
	var target, thread uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT session_id,thread_id FROM slack_requests WHERE id=$1 AND status='accepted'`, first).Scan(&target, &thread); err != nil {
		t.Fatal("Agent use could not start shared work", err)
	}
	var shared bool
	if err := f.Pool.QueryRow(ctx, `SELECT origin_slack_connection_id=$2 FROM sessions WHERE id=$1`, target, f.publication).Scan(&shared); err != nil || !shared {
		t.Fatal("shared runtime borrowed personal origin", shared, err)
	}
	// Slack participation never becomes Console access to all conversation history.
	if _, err := session.GetSession(ctx, f.Pool, session.Caller{Kind: "user", ID: f.User}, f.Environment, target); !errors.Is(err, session.ErrDenied) {
		t.Fatal("channel participation became Console history access", err)
	}
	page, err := session.ListSessions(ctx, f.Pool, session.Caller{Kind: "user", ID: f.User}, session.SessionListRequest{EnvironmentID: f.Environment, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range page.Sessions {
		if view.ID == target {
			t.Fatal("shared creator gained personal list access")
		}
	}
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,uuidv7(),$2,'local','personal','user',$4),($1,uuidv7(),$3,'local','shared','user',$4)`, f.Environment, f.Session, target, f.User)
	attention, err := session.ListAttention(ctx, f.Pool, session.Caller{Kind: "user", ID: f.User}, f.Environment)
	if err != nil || attention.HoldCount != 1 || len(attention.Holds) != 1 || attention.Holds[0].SessionID != f.Session {
		t.Fatalf("shared creator gained personal attention access: %+v %v", attention, err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM session_holds WHERE environment_id=$1`, f.Environment)
	peer := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Peer'); INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$1 FROM environments WHERE id=$2; INSERT INTO slack_user_links(team_id,slack_user_id,user_id) VALUES('team','peer',$1)`, pgx.QueryExecModeSimpleProtocol, peer, f.Environment)
	var org uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	member := dbtest.GrantMember(t, f.Pool, org, peer, &f.Environment, "agents.use")
	// Losing the creator's grant does not transfer administrative authority or stop
	// an independently authorized participant from using the shared connection.
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.User)
	send := func(key string) uuid.UUID {
		t.Helper()
		payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Thread: root, Text: "<@bot> continue"})
		now := time.Now().UTC()
		r, err := receiveGesture(ctx, f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "peer", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if err := admitMessage(ctx, f.Pool, nil, f.admissionClient(t), r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	second := send("shared-peer")
	var accepted bool
	if err := f.Pool.QueryRow(ctx, `SELECT status='accepted' AND session_id=$2 FROM slack_requests WHERE id=$1`, second, target).Scan(&accepted); err != nil || !accepted {
		t.Fatal("eligible peer could not continue", accepted, err)
	}
	// A private Console session projected into Slack remains private to its owner
	// and explicitly assigned operators; the shared route does not convert it.
	input := session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "copied-private", Input: json.RawMessage(`[]`)}
	if _, err := session.RouteSlackInput(ctx, f.Pool, nil, session.Caller{Kind: "user", ID: peer, SlackThreadID: f.thread}, input); !errors.Is(err, session.ErrDenied) {
		t.Fatal("private projection became shared", err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM role_grants WHERE member_id=$1`, member)
	denied := send("shared-revoked")
	if err := f.Pool.QueryRow(ctx, `SELECT status='rejected' AND error='permission_denied' FROM slack_requests WHERE id=$1`, denied).Scan(&accepted); err != nil || !accepted {
		t.Fatal("revoked peer admitted", accepted, err)
	}
}

func TestSlackQueuedInputRejectsRemovedChannelParticipant(t *testing.T) {
	f := newStatusFixture(t)
	ctx := t.Context()
	f.link(t)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1; UPDATE roles SET permissions=ARRAY['agents.use']`, pgx.QueryExecModeSimpleProtocol, f.Session, f.publication)
	request := f.reply(t, "queued-participant", false)
	if err := admitMessage(ctx, f.Pool, nil, f.admissionClient(t), request); err != nil {
		t.Fatal(err)
	}
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	result, err := session.DispatchRuntimeTurn(ctx, f.Pool, host, e, 1, fixtureConversationVerifier(false))
	if err != nil || result != nil {
		t.Fatalf("removed participant dispatched: %+v %v", result, err)
	}
	var rejected bool
	if err := f.Pool.QueryRow(ctx, `SELECT t.status='failed' AND t.error_code='permission_revoked' FROM turns t JOIN slack_requests r ON r.turn_id=t.id WHERE r.id=$1`, request).Scan(&rejected); err != nil || !rejected {
		t.Fatal("pending input not rejected", rejected, err)
	}
}

func TestSharedRuntimeCannotAttachToExistingComputer(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1`, f.Session, f.publication)
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	caller := session.Caller{Kind: "session", ID: f.Session, Execution: e, Host: &workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	for _, method := range []string{"create", "spawn"} {
		req := session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", ComputerID: f.Computer, IdempotencyKey: method, Input: json.RawMessage(`[]`)}
		start := session.Start
		if method == "spawn" {
			start = session.Spawn
		}
		if _, err := start(t.Context(), f.Pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
			t.Fatalf("shared %s attached Computer: %v", method, err)
		}
	}
}

type callbackConversationVerifier func() (bool, error)

func (v callbackConversationVerifier) VerifyInputConversation(context.Context, uuid.UUID, int64, string, string, string) (bool, error) {
	return v()
}

func TestSlackQueuedInputRechecksExactActorLinkAfterVerification(t *testing.T) {
	for _, mode := range []string{"scoped", "shared"} {
		t.Run(mode, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			if mode == "shared" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1`, f.Session, f.publication)
			}
			request := f.reply(t, "queued-unlinked", false)
			if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), request); err != nil {
				t.Fatal(err)
			}
			verifier := callbackConversationVerifier(func() (bool, error) {
				_, err := f.Pool.Exec(t.Context(), `DELETE FROM slack_user_links WHERE user_id=$1`, f.User)
				return true, err
			})
			e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			result, err := session.DispatchRuntimeTurn(t.Context(), f.Pool, host, e, 1, verifier)
			if err != nil || result != nil {
				t.Fatalf("unlinked input dispatched: %+v %v", result, err)
			}
			var rejected bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT t.status='failed' AND t.error_code='permission_revoked' FROM turns t JOIN slack_requests r ON r.turn_id=t.id WHERE r.id=$1`, request).Scan(&rejected); err != nil || !rejected {
				t.Fatal("input not rejected", rejected, err)
			}
		})
	}
}

func TestSlackQueuedInputWaitsBeforeExternalVerification(t *testing.T) {
	for _, blocked := range []string{"hold", "storage", "control", "attachment", "temporary", "unconfigured"} {
		t.Run(blocked, func(t *testing.T) {
			f := newStatusFixture(t)
			ctx := t.Context()
			f.link(t)
			request := f.reply(t, "queued-wait", false)
			if err := admitMessage(ctx, f.Pool, nil, f.admissionClient(t), request); err != nil {
				t.Fatal(err)
			}
			e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			attachment := int64(1)
			switch blocked {
			case "hold":
				dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,uuidv7(),$2,'local','wait','user',$3)`, f.Environment, f.Session, f.User)
			case "storage":
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp() WHERE computer_id=$1`, f.Computer)
			case "control":
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET control_acknowledged_at=NULL WHERE session_id=$1`, f.Session)
			case "attachment":
				attachment = 2
			}
			calls := 0
			verifier := callbackConversationVerifier(func() (bool, error) { calls++; return false, errors.New("provider rate limited") })
			var inputVerifier session.ConversationVerifier = verifier
			if blocked == "unconfigured" {
				inputVerifier = nil
			}
			result, err := session.DispatchRuntimeTurn(ctx, f.Pool, host, e, attachment, inputVerifier)
			if blocked == "unconfigured" && !errors.Is(err, session.ErrConversationVerificationUnavailable) {
				t.Fatal("missing configuration diagnostic", err)
			}
			if result != nil || !(errors.Is(err, session.ErrNotReady) || errors.Is(err, session.ErrDenied)) {
				t.Fatalf("wait: %+v %v", result, err)
			}
			wantCalls := 0
			if blocked == "temporary" {
				wantCalls = 1
				if !errors.Is(err, session.ErrNotReady) {
					t.Fatal("temporary failure must be retryable", err)
				}
			}
			if calls != wantCalls {
				t.Fatalf("verification calls=%d want %d", calls, wantCalls)
			}
			var queued bool
			if err := f.Pool.QueryRow(ctx, `SELECT t.status='queued' FROM turns t JOIN slack_requests r ON r.turn_id=t.id WHERE r.id=$1`, request).Scan(&queued); err != nil || !queued {
				t.Fatal("input lost while waiting", queued, err)
			}
			dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM session_holds WHERE session_id=$1; UPDATE session_processes SET control_acknowledged_at=clock_timestamp() WHERE session_id=$1; UPDATE computer_leases SET storage_pressure_since=NULL WHERE computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.Session, f.Computer)
			result, err = session.DispatchRuntimeTurn(ctx, f.Pool, host, e, 1, fixtureConversationVerifier(true))
			if err != nil || result == nil {
				t.Fatalf("retry did not dispatch: %+v %v", result, err)
			}
		})
	}
}

func TestSharedRuntimeDiscoveryKeepsRelationshipsAndScheduleIsolation(t *testing.T) {
	f := newStatusFixture(t)
	ctx := t.Context()
	f.link(t)
	other, channel, thread := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,causal_depth,origin_user_id,origin_slack_connection_id) SELECT environment_id,$3,agent_id,deployment_id,$3,0,origin_user_id,$2 FROM sessions WHERE id=$1;
 INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $4,environment_id,connection_id,installation_id,org_id,team_id,'C2' FROM slack_channel_bindings WHERE id=$5;
 INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key) SELECT $6,environment_id,$3,$4,org_id,team_id,'C2','987.654','other-opening' FROM slack_threads WHERE id=$7`, pgx.QueryExecModeSimpleProtocol, f.Session, f.publication, other, channel, f.channel, thread, f.thread)
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	caller := session.Caller{Kind: "session", ID: f.Session, Execution: e, Host: &workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	for _, relation := range []string{"owned", "requested"} {
		page, err := session.RuntimeListSessions(ctx, f.Pool, caller, session.RuntimeSessionListRequest{Relation: relation, Limit: 100})
		if err != nil || len(page.Sessions) != 0 {
			t.Fatalf("unrelated channel listed: %+v %v", page, err)
		}
	}
	if _, err := session.RuntimeGetSession(ctx, f.Pool, caller, other); !errors.Is(err, session.ErrDenied) {
		t.Fatal("unrelated shared runtime read", err)
	}
	// Eligible participants can control their shared conversation without an
	// administrative grant over other personal conversations.
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE roles SET permissions=ARRAY['agents.use']`)
	for _, operation := range []auth.Permission{auth.PermissionSessionsClose, auth.PermissionSessionsCancel, auth.PermissionSessionsResume} {
		err := db.RunTx(ctx, f.Pool, func(tx pgx.Tx) error {
			return session.AuthorizeSessionOperation(ctx, tx, session.Caller{Kind: "user", ID: f.User, SlackThreadID: f.thread}, f.Environment, f.Session, operation)
		})
		if err != nil {
			t.Fatalf("participant %s: %v", operation, err)
		}
	}
	activation := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) SELECT environment_id,agent_id,deployment_id,'daily','* * * * *','UTC','[]' FROM sessions WHERE id=$1;
 INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at) SELECT environment_id,$2,agent_id,deployment_id,'daily',clock_timestamp(),clock_timestamp() FROM sessions WHERE id=$1;
 UPDATE sessions SET origin_user_id=NULL,origin_slack_connection_id=NULL,origin_activation_id=$2 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Session, activation)
	for _, operation := range []auth.Permission{auth.PermissionSessionsRead, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsAnswer} {
		err := db.RunTx(ctx, f.Pool, func(tx pgx.Tx) error {
			return session.AuthorizeSessionOperation(ctx, tx, caller, f.Environment, other, operation)
		})
		if !errors.Is(err, session.ErrDenied) {
			t.Fatalf("schedule reached shared target %s: %v", operation, err)
		}
	}
	for _, relation := range []string{"owned", "requested"} {
		page, err := session.RuntimeListSessions(ctx, f.Pool, caller, session.RuntimeSessionListRequest{Relation: relation, Limit: 100})
		if err != nil || len(page.Sessions) != 0 {
			t.Fatalf("schedule listed unrelated channel: %+v %v", page, err)
		}
	}
}
