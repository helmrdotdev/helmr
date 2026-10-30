package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func scopeFixtureToAgent(t *testing.T, f fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE roles SET permissions=ARRAY['sessions.create','sessions.read','sessions.send','sessions.answer','sessions.resume','sessions.interrupt','sessions.cancel','sessions.close'] WHERE id IN (SELECT role_id FROM role_grants WHERE environment_id=$1);
 UPDATE role_grants SET all_agents=false WHERE environment_id=$1; INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.agent)
}

func TestSessionCreateReceiptAndExplicitAgentActions(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE roles SET permissions=ARRAY['sessions.create'] WHERE id IN (SELECT role_id FROM role_grants WHERE environment_id=$1)`, f.env)
	req := f.startRequest("create-only")
	req.ComputerID = f.computer
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("creation granted existing Computer placement", err)
	}
	req.ComputerID = uuid.Nil()
	key := "issue:123"
	req.SessionKey = &key
	first, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal("create-only keyed admission", err)
	}
	if replay, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); err != nil || replay.SessionID != first.SessionID {
		t.Fatal("creation receipt replay", replay, err)
	}
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, first.SessionID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("create-only disclosed own history", err)
	}
	page, err := session.ListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 1})
	if err != nil || len(page.Sessions) != 0 {
		t.Fatal("create-only list disclosed history", page, err)
	}
	if _, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: "interrupt", IdempotencyKey: "create-only-stop"}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("origin implied control", err)
	}
	other := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Peer'); INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$1 FROM environments WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, other, f.env)
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, org, other, &f.env, "sessions.read", "sessions.send", "sessions.interrupt")
	peer := session.UserCaller((sessiontest.Fixture{Pool: f.pool, User: other, Environment: f.env}).Principal(t))
	view, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, peer, f.env, first.SessionID)
	if err != nil {
		t.Fatal("other member read denied", err)
	}
	if len(view.Permissions) != 3 {
		t.Fatalf("capability projection: %v", view.Permissions)
	}
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, peer, f.startRequest("peer-create")); !errors.Is(err, session.ErrDenied) {
		t.Fatal("actions implied creation", err)
	}
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, peer, session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "peer-input", Input: req.Input}); err != nil {
		t.Fatal("send required create or matching origin", err)
	}
	if _, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, peer, session.SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: "interrupt", IdempotencyKey: "peer-stop"}); err != nil {
		t.Fatal("cross-user interrupt", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked creation receipt exposed", err)
	}
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, peer, f.env, first.SessionID); err != nil {
		t.Fatal("origin revocation hid other reader's history", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE roles SET permissions=ARRAY['sessions.read'] WHERE id IN (SELECT g.role_id FROM role_grants g JOIN org_members m ON m.id=g.member_id WHERE m.user_id=$1)`, other)
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, peer, session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "revoked-send", Input: req.Input}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked send admitted", err)
	}
}

func TestScopedRuntimeRelationsAndQueuedRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET requester_session_id=NULL,causal_depth=0 WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	peer := f.peer(t)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET requester_session_id=NULL,causal_depth=0 WHERE environment_id=$1 AND id=$2`, f.env, peer.session)
	caller := runtimeReadCaller(t, f)
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("same-origin peer readable", err)
	}
	if _, _, err := session.RuntimeListComputers(ctx, f.pool, caller, computer.ListPage{Limit: 10}); err != nil {
		t.Fatal("scoped runtime listed Computers", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, peer.session)
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, peer.session); err != nil {
		t.Fatal("direct requested root denied", err)
	}
	// Accepted input and runtime reads survive the original user losing access.
	queued := peer.enqueue(t, "queued")
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := dispatchTurnForTest(ctx, f.pool, peer.execution()); err != nil {
		t.Fatal("revoked Turn dispatched", err)
	}
	var failed bool
	if err := f.pool.QueryRow(ctx, `SELECT status='running' AND processing_closed_at IS NULL FROM turns WHERE id=$1`, queued.TurnID).Scan(&failed); err != nil || !failed {
		t.Fatal("denied queue not settled", failed, err)
	}
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, peer.session); err != nil {
		t.Fatal("accepted runtime read denied", err)
	}
}

func TestScopedAPIKeyIdentityAndLineageNeverUpgrade(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	key, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash)
 SELECT k.id,e.org_id,e.project_id,e.id,k.name,'fixture',convert_to(k.name,'UTF8') FROM environments e CROSS JOIN (VALUES($1::uuid,'source'),($2::uuid,'other')) k(id,name) WHERE e.id=$3`, key, other, f.env)
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.create", "sessions.read")
	dbtest.SetAPIKeyRole(t, f.pool, other, "sessions.create", "sessions.read")
	caller := session.Caller{Kind: "api_key", ID: key}
	req := f.startRequest("machine")
	noRetry := req
	noRetry.IdempotencyKey = ""
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, noRetry); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatal("scoped key did not require retry key", err)
	}
	created, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	var personal bool
	if err = f.pool.QueryRow(ctx, `SELECT origin_slack_connection_id IS NULL FROM sessions WHERE id=$1 AND origin_api_key_id=$2 AND origin_user_id IS NULL`, created.SessionID, key).Scan(&personal); err != nil || !personal {
		t.Fatal("run viewer upgraded lineage", personal, err)
	}
	// Token rotation leaves the machine principal ID and its receipt ownership intact.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET token_hash=convert_to('rotated','UTF8') WHERE id=$1`, key)
	if replay, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req); err != nil || replay.SessionID != created.SessionID {
		t.Fatal("rotation lost receipt", replay, err)
	}
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, session.Caller{Kind: "api_key", ID: other}, session.EnqueueRequest{EnvironmentID: f.env, SessionID: created.SessionID, IdempotencyKey: "peer", Input: req.Input}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("different key controlled scoped run", err)
	}
	// Exercise an authenticated runtime on an existing fixture attachment.
	peer := f.peer(t)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET origin_user_id=NULL,origin_api_key_id=$1 WHERE id=$2`, key, f.session)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET origin_user_id=NULL,origin_api_key_id=$1 WHERE id=$2`, key, peer.session)
	runtime := runtimeReadCaller(t, f)
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.read")
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, peer.session); err != nil {
		t.Fatal("explicit run-view path denied", err)
	}
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, runtime, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("later elevation widened scoped runtime", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked key returned receipt", err)
	}
	if _, err := session.RuntimeGetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, runtime, f.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked key runtime remained authorized", err)
	}
}

func TestScopedRuntimeCreationPreservesOriginAndComputerBoundary(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET requester_session_id=NULL,causal_depth=0 WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	admitted := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(ctx, f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := session.Caller{Kind: "session", MCPScopeID: f.mcpScope, ID: f.session, TurnID: admitted.TurnID, Execution: f.execution(), Host: f.host()}
	for _, method := range []string{"create", "spawn"} {
		result, err := session.AdmitSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, f.startRequest("scoped-"+method), method)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var valid bool
		if err := f.pool.QueryRow(ctx, `SELECT origin_slack_connection_id IS NULL AND origin_user_id IS NOT NULL AND origin_user_id=$3 AND requester_session_id=$4 AND CASE WHEN $5='spawn' THEN computer_id<>$6 AND parent_session_id=$4 ELSE computer_id<>$6 AND parent_session_id IS NULL END FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, result.SessionID, f.user, f.session, method, f.computer).Scan(&valid); err != nil || !valid {
			t.Fatalf("%s origin/isolation: %v %v", method, valid, err)
		}
	}
	helper := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'helper'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3`, pgx.QueryExecModeSimpleProtocol, f.env, helper, f.agent)
	req := f.startRequest("helper-denied")
	req.Agent = "helper"
	if _, err := session.Spawn(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("ungranted helper admitted: %v", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := session.Spawn(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, f.startRequest("revoked")); err != nil {
		t.Fatalf("accepted runtime could not spawn: %v", err)
	}
}

func TestAPIKeySelectedAgentSessionVisibilityAndRevocation(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	key, hiddenAgent := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) SELECT $1,org_id,project_id,id,'selected','fixture',convert_to('selected','UTF8') FROM environments WHERE id=$2`, key, f.env)
	dbtest.SetAPIKeyRole(t, f.pool, key, "sessions.create", "sessions.read")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE role_grants SET all_agents=false WHERE api_key_id=$1; INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE api_key_id=$1`, pgx.QueryExecModeSimpleProtocol, key, f.agent)
	hidden := f.peer(t)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'hidden'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3; UPDATE sessions SET agent_id=$2 WHERE id=$4`, pgx.QueryExecModeSimpleProtocol, f.env, hiddenAgent, f.agent, hidden.session)
	caller := session.Caller{Kind: "api_key", ID: key}
	page, err := session.ListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionListRequest{EnvironmentID: f.env, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session {
		t.Fatalf("selected visibility: %+v %v", page, err)
	}
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, hidden.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("hidden session readable", err)
	}
	req := f.startRequest("hidden-key")
	req.Agent = "hidden"
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("hidden Agent admitted", err)
	}
	if _, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, f.startRequest("selected-key")); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grants WHERE api_key_id=$1`, key)
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, f.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("removed assignment still authorizes", err)
	}
}

func TestAcceptedRuntimeCanContinueChildrenAfterAccessRevocation(t *testing.T) {
	for _, method := range []string{"create", "spawn"} {
		t.Run(method, func(t *testing.T) {
			f := newAdmissionFixture(t)
			caller := runtimeReadCaller(t, f)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.user)
			result, err := session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, f.startRequest("after-logout"), method)
			if err != nil {
				t.Fatal("logout blocked the accepted runtime", err)
			}
			var inherited bool
			if err = f.pool.QueryRow(t.Context(), `SELECT child.computer_id<>parent.computer_id AND child.origin_user_id=parent.origin_user_id FROM sessions child JOIN sessions parent ON parent.environment_id=child.environment_id WHERE child.environment_id=$1 AND child.id=$2 AND parent.id=$3`, f.env, result.SessionID, f.session).Scan(&inherited); err != nil || !inherited {
				t.Fatal(inherited, err)
			}
			// New human requests do not inherit the runtime's continuing authority.
			if _, err = session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("new-human")); !errors.Is(err, session.ErrDenied) {
				t.Fatal("logged-out human started new work", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1`, f.user)
			if _, err = session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, f.startRequest("after-incident"), method); err != nil {
				t.Fatal("account revocation blocked accepted runtime", err)
			}
		})
	}
}

func TestSessionKeyNamespaceIsSharedAcrossPrincipalsAndClosure(t *testing.T) {
	f := newAdmissionFixture(t)
	firstCaller := askKey(t, f, []string{"sessions.create"})
	secondCaller := askKey(t, f, []string{"sessions.create"})
	req := f.startRequest("first-keyed")
	req.ComputerID = uuid.Nil()
	key := "ticket:42"
	req.SessionKey = &key
	first, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, firstCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.IdempotencyKey = "second-keyed"
	if _, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, secondCaller, other); !errors.Is(err, session.ErrConflict) {
		t.Fatal("principal-specific key namespace", err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: "close", IdempotencyKey: "close-keyed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, secondCaller, other); !errors.Is(err, session.ErrConflict) {
		t.Fatal("closed key reused", err)
	}
	replay, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, firstCaller, req)
	if err != nil || replay.SessionID != first.SessionID {
		t.Fatal("create-only receipt after closure", replay, err)
	}
}
