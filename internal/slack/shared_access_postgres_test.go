package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSlackSharedConversationUsesExplicitSessionActions(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	ctx := t.Context()
	f.link(t)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE roles SET permissions=ARRAY['sessions.create']`)
	root := slackNow()
	first := f.newMessage(t, "shared-start", root)
	if err := admitMessage(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), first); err != nil {
		t.Fatal(err)
	}
	var target, thread uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT session_id,thread_id FROM slack_requests WHERE id=$1 AND status='accepted'`, first).Scan(&target, &thread); err != nil {
		t.Fatal("Create permission could not start shared work", err)
	}
	var shared bool
	if err := f.Pool.QueryRow(ctx, `SELECT origin_slack_connection_id=$2 FROM sessions WHERE id=$1`, target, f.publication).Scan(&shared); err != nil || !shared {
		t.Fatal("shared runtime borrowed personal origin", shared, err)
	}
	// Slack participation never becomes Console access to all conversation history.
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), f.Environment, target); !errors.Is(err, session.ErrDenied) {
		t.Fatal("channel participation became Console history access", err)
	}
	page, err := session.ListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionListRequest{EnvironmentID: f.Environment, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range page.Sessions {
		if view.ID == target {
			t.Fatal("shared creator gained personal list access")
		}
	}
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,uuidv7(),$2,'local','personal','user',$4),($1,uuidv7(),$3,'local','shared','user',$4)`, f.Environment, f.Session, target, f.User)
	attention, err := session.ListAttention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), f.Environment)
	if !errors.Is(err, session.ErrDenied) || attention.HoldCount != 0 || len(attention.Holds) != 0 {
		t.Fatalf("shared creator gained personal attention access: %+v %v", attention, err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM session_holds WHERE environment_id=$1`, f.Environment)
	peer := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Peer'); INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$1 FROM environments WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, peer, f.Environment)
	var org uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	member := dbtest.GrantMember(t, f.Pool, org, peer, &f.Environment, "sessions.send")
	f.linkActor(t, peer, "peer")
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
		if err := admitMessage(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	second := send("shared-peer")
	var accepted bool
	if err := f.Pool.QueryRow(ctx, `SELECT status='accepted' AND session_id=$2 FROM slack_requests WHERE id=$1`, second, target).Scan(&accepted); err != nil || !accepted {
		t.Fatal("eligible peer could not continue", accepted, err)
	}

	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM role_grants WHERE member_id=$1`, member)
	denied := send("shared-revoked")
	if err := f.Pool.QueryRow(ctx, `SELECT status='rejected' AND error='permission_denied' FROM slack_requests WHERE id=$1`, denied).Scan(&accepted); err != nil || !accepted {
		t.Fatal("revoked peer admitted", accepted, err)
	}
}

func TestSharedRuntimeCannotAttachToUnrelatedComputer(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1`, f.Session, f.publication)
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
	caller := session.Caller{Kind: "session", MCPScopeID: f.MCPScope, ID: f.Session, Execution: e, Host: &workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	for _, method := range []string{"create", "spawn"} {
		req := session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", ComputerID: uuid.NewV7(), IdempotencyKey: method, Input: json.RawMessage(`[]`)}
		start := session.Start
		if method == "spawn" {
			start = session.Spawn
		}
		if _, err := start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
			t.Fatalf("shared %s attached Computer: %v", method, err)
		}
	}
}

func TestSlackAcceptedInputSurvivesAccessRevocation(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, mutation := range []string{
			"UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE user_id=$1",
			"UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1",
			"UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE user_id=$1",
			"UPDATE org_members SET suspended_at=clock_timestamp() WHERE user_id=$1",
			"UPDATE slack_installations SET authorization_lost_at=clock_timestamp()",
		} {
			t.Run(fmt.Sprintf("shared=%v/%s", shared, mutation), func(t *testing.T) {
				f := newStatusFixture(t)
				f.link(t)
				if shared {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET origin_slack_connection_id=$2 WHERE id=$1`, f.Session, f.publication)
				}
				request := f.reply(t, "accepted-revocation", false)
				if err := admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), request); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(mutation, "$1") {
					dbtest.MustExec(t, t.Context(), f.Pool, mutation, f.User)
				} else {
					dbtest.MustExec(t, t.Context(), f.Pool, mutation)
				}
				e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
				host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
				result, err := session.DispatchRuntimeTurn(t.Context(), f.Pool, host, e, 1)
				if err != nil || result == nil {
					t.Fatalf("accepted input lost: %+v %v", result, err)
				}
			})
		}
	}
}

func TestSlackAcceptedInputRespectsWorkAndPhysicalControls(t *testing.T) {
	for _, blocked := range []string{"hold", "storage", "control", "attachment"} {
		t.Run(blocked, func(t *testing.T) {
			f := newStatusFixture(t)
			ctx := t.Context()
			f.link(t)
			request := f.reply(t, "queued-wait", false)
			if err := admitMessage(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), request); err != nil {
				t.Fatal(err)
			}
			e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
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
			result, err := session.DispatchRuntimeTurn(ctx, f.Pool, host, e, attachment)
			if result != nil || (err != nil && !errors.Is(err, session.ErrNotReady) && !errors.Is(err, session.ErrDenied)) {
				t.Fatalf("wait: %+v %v", result, err)
			}
			var queued bool
			if err := f.Pool.QueryRow(ctx, `SELECT t.status='queued' FROM turns t JOIN slack_requests r ON r.turn_id=t.id WHERE r.id=$1`, request).Scan(&queued); err != nil || !queued {
				t.Fatal("input lost while waiting", queued, err)
			}
			dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM session_holds WHERE session_id=$1; UPDATE session_processes SET control_acknowledged_at=clock_timestamp() WHERE session_id=$1; UPDATE computer_leases SET storage_pressure_since=NULL WHERE computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.Session, f.Computer)
			result, err = session.DispatchRuntimeTurn(ctx, f.Pool, host, e, 1)
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
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
	caller := session.Caller{Kind: "session", MCPScopeID: f.MCPScope, ID: f.Session, Execution: e, Host: &workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	for _, relation := range []string{"owned", "requested"} {
		page, err := session.RuntimeListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, session.RuntimeSessionListRequest{Relation: relation, Limit: 100})
		if err != nil || len(page.Sessions) != 0 {
			t.Fatalf("unrelated channel listed: %+v %v", page, err)
		}
	}
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, other); !errors.Is(err, session.ErrDenied) {
		t.Fatal("unrelated shared runtime read", err)
	}
	// Slack participation does not imply control; explicit actions apply to the Agent.
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE roles SET permissions=ARRAY['sessions.close','sessions.cancel','sessions.resume']`)
	for _, operation := range []auth.Permission{auth.PermissionSessionsClose, auth.PermissionSessionsCancel, auth.PermissionSessionsResume} {
		err := db.RunTx(ctx, f.Pool, func(tx pgx.Tx) error {
			human := session.UserCaller(f.Principal(t))
			return session.AuthorizeSessionOperation(ctx, tx, human, f.Environment, f.Session, operation)
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
		page, err := session.RuntimeListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, caller, session.RuntimeSessionListRequest{Relation: relation, Limit: 100})
		if err != nil || len(page.Sessions) != 0 {
			t.Fatalf("schedule listed unrelated channel: %+v %v", page, err)
		}
	}
}
