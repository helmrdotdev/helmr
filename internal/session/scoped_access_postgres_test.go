package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func scopeFixtureToAgent(t *testing.T, f fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM role_permissions WHERE role_id IN (SELECT role_id FROM role_grants WHERE environment_id=$1);
 INSERT INTO role_permissions(org_id,role_id,permission) SELECT org_id,role_id,'agents.use' FROM role_grants WHERE environment_id=$1;
 UPDATE role_grants SET agents_mode='selected' WHERE environment_id=$1; INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.agent)
}

func TestScopedSessionOriginIsolationAndCurrentAccess(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	req := f.startRequest("scoped")
	req.ComputerID = f.computer
	if _, err := session.Start(ctx, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("scoped Computer attachment accepted", err)
	}
	req.ComputerID = uuid.Nil()
	key := "shared-name"
	req.SessionKey = &key
	if _, err := session.Start(ctx, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("scoped keyed start accepted", err)
	}
	req.SessionKey = nil
	first, err := session.Start(ctx, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	var fresh bool
	if err = f.pool.QueryRow(ctx, `SELECT admission_mode='scoped' AND origin_kind='user' AND origin_user_id=$3 AND computer_id<>$4 FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, first.SessionID, f.user, f.computer).Scan(&fresh); err != nil || !fresh {
		t.Fatal("invalid scoped origin or Computer", fresh, err)
	}
	if _, err = session.Start(ctx, f.pool, nil, f.caller(), req); err != nil {
		t.Fatal("own creation retry failed", err)
	}
	if _, err = session.GetSession(ctx, f.pool, f.caller(), f.env, f.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("Use read an environment Session", err)
	}
	other := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Peer');
 INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$1 FROM environments WHERE id=$2;
 INSERT INTO permission_group_members(org_id,group_id,member_id) SELECT gm.org_id,gm.group_id,other.id FROM permission_group_members gm JOIN org_members existing ON existing.id=gm.member_id JOIN org_members other ON other.org_id=gm.org_id AND other.user_id=$1 WHERE existing.user_id=$3`, pgx.QueryExecModeSimpleProtocol, other, f.env, f.user)
	peer := session.Caller{Kind: "user", ID: other}
	if _, err = session.GetSession(ctx, f.pool, peer, f.env, first.SessionID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("peer read own-origin session", err)
	}
	for _, tc := range []struct {
		caller session.Caller
		denied bool
	}{{f.caller(), false}, {peer, true}} {
		err := db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
			scope, err := session.SessionCloseScope(ctx, tx, tc.caller, f.env, first.SessionID)
			if err == nil && (len(scope.Sessions) != 1 || scope.Sessions[0] != first.SessionID.String()) {
				t.Fatalf("unexpected close scope: %+v", scope)
			}
			return err
		})
		if tc.denied && !errors.Is(err, session.ErrDenied) || !tc.denied && err != nil {
			t.Fatalf("close scope denied=%v: %v", tc.denied, err)
		}
	}
	second, err := session.Start(ctx, f.pool, nil, peer, f.startRequest("peer"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := session.ListSessions(ctx, f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, Limit: 1})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != first.SessionID || page.NextCursor != uuid.Nil() {
		t.Fatal("filter ran after pagination", page, err)
	}
	if _, err = session.Enqueue(ctx, f.pool, nil, peer, session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "peer-input", Input: req.Input}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("peer input accepted", err)
	}
	// A trusted environment operator can intervene, but cannot attach to the scoped Computer.
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, org, other, &f.env, "runs.operate", "computers.operate")
	view, err := session.GetSession(ctx, f.pool, peer, f.env, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	attachment := f.startRequest("operator-attach")
	attachment.ComputerID = *view.ComputerID
	if _, err = session.Start(ctx, f.pool, nil, peer, attachment); !errors.Is(err, session.ErrDenied) {
		t.Fatal("operator attached a peer to scoped Computer", err)
	}
	if _, err = session.ControlSession(ctx, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: first.SessionID, Kind: "interrupt", IdempotencyKey: "own-stop"}); err != nil {
		t.Fatal("own scoped control failed", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err = session.Start(ctx, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked creation receipt exposed", err)
	}
	if _, err = session.GetSession(ctx, f.pool, f.caller(), f.env, first.SessionID); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked read accepted", err)
	}
	if _, err = session.GetSession(ctx, f.pool, peer, f.env, second.SessionID); err != nil {
		t.Fatal("independent operator path lost", err)
	}
}

func TestScopedRuntimeRelationsAndQueuedRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET admission_mode='scoped' WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	peer := f.peer(t)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET admission_mode='scoped' WHERE environment_id=$1 AND id=$2`, f.env, peer.session)
	caller := runtimeReadCaller(t, f)
	if _, err := session.RuntimeGetSession(ctx, f.pool, caller, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("same-origin peer readable", err)
	}
	if _, _, err := session.RuntimeListComputers(ctx, f.pool, caller, computer.ListPage{Limit: 10}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("scoped runtime listed Computers", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, peer.session)
	if _, err := session.RuntimeGetSession(ctx, f.pool, caller, peer.session); err != nil {
		t.Fatal("direct requested root denied", err)
	}
	// Revoking the origin prevents a fresh queued Turn and records its failure.
	queued := peer.enqueue(t, "queued")
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := dispatchTurnForTest(ctx, f.pool, peer.execution()); !errors.Is(err, session.ErrNotReady) {
		t.Fatal("revoked Turn dispatched", err)
	}
	var failed bool
	if err := f.pool.QueryRow(ctx, `SELECT status='failed' AND error_code='permission_revoked' AND processing_closed_at IS NOT NULL FROM turns WHERE id=$1`, queued.TurnID).Scan(&failed); err != nil || !failed {
		t.Fatal("denied queue not settled", failed, err)
	}
	if _, err := session.RuntimeGetSession(ctx, f.pool, caller, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked runtime read accepted", err)
	}
}

func TestScopedAPIKeyIdentityAndLineageNeverUpgrade(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	key, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,permissions,name,key_prefix,token_hash)
 SELECT k.id,e.org_id,e.project_id,e.id,ARRAY['agents.use','runs.view'],k.name,'fixture',convert_to(k.name,'UTF8') FROM environments e CROSS JOIN (VALUES($1::uuid,'source'),($2::uuid,'other')) k(id,name) WHERE e.id=$3`, key, other, f.env)
	caller := session.Caller{Kind: "api_key", ID: key}
	req := f.startRequest("machine")
	noRetry := req
	noRetry.IdempotencyKey = ""
	if _, err := session.Start(ctx, f.pool, nil, caller, noRetry); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatal("scoped key did not require retry key", err)
	}
	created, err := session.Start(ctx, f.pool, nil, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err = f.pool.QueryRow(ctx, `SELECT admission_mode FROM sessions WHERE id=$1 AND origin_api_key_id=$2 AND origin_user_id IS NULL`, created.SessionID, key).Scan(&mode); err != nil || mode != "scoped" {
		t.Fatal("run viewer upgraded lineage", mode, err)
	}
	// Token rotation leaves the machine principal ID and its receipt ownership intact.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET token_hash=convert_to('rotated','UTF8') WHERE id=$1`, key)
	if replay, err := session.Start(ctx, f.pool, nil, caller, req); err != nil || replay.SessionID != created.SessionID {
		t.Fatal("rotation lost receipt", replay, err)
	}
	if _, err := session.Enqueue(ctx, f.pool, nil, session.Caller{Kind: "api_key", ID: other}, session.EnqueueRequest{EnvironmentID: f.env, SessionID: created.SessionID, IdempotencyKey: "peer", Input: req.Input}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("different key controlled scoped run", err)
	}
	// Exercise an authenticated runtime on an existing fixture attachment.
	peer := f.peer(t)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET admission_mode='scoped',origin_kind='api_key',origin_user_id=NULL,origin_api_key_id=$1 WHERE id=$2`, key, f.session)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET admission_mode='scoped',origin_kind='api_key',origin_user_id=NULL,origin_api_key_id=$1 WHERE id=$2`, key, peer.session)
	runtime := runtimeReadCaller(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET permissions=ARRAY['runs.operate'] WHERE id=$1`, key)
	if _, err := session.GetSession(ctx, f.pool, caller, f.env, peer.session); err != nil {
		t.Fatal("explicit run-view path denied", err)
	}
	if _, err := session.RuntimeGetSession(ctx, f.pool, runtime, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("later elevation widened scoped runtime", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	if _, err := session.Start(ctx, f.pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked key returned receipt", err)
	}
	if _, err := session.RuntimeGetSession(ctx, f.pool, runtime, f.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("revoked key runtime remained authorized", err)
	}
}

func TestScopedRuntimeCreationPreservesOriginAndComputerBoundary(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	scopeFixtureToAgent(t, f)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET admission_mode='scoped' WHERE environment_id=$1 AND id=$2`, f.env, f.session)
	admitted := f.enqueue(t, "caller")
	if _, err := dispatchTurnForTest(ctx, f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	caller := session.Caller{Kind: "session", ID: f.session, TurnID: admitted.TurnID, Execution: f.execution(), Host: f.host()}
	for _, method := range []string{"create", "spawn"} {
		result, err := session.AdmitSession(ctx, f.pool, nil, caller, f.startRequest("scoped-"+method), method)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var valid bool
		if err := f.pool.QueryRow(ctx, `SELECT admission_mode='scoped' AND origin_kind='user' AND origin_user_id=$3 AND requester_session_id=$4 AND CASE WHEN $5='spawn' THEN computer_id<>$6 AND parent_session_id=$4 ELSE computer_id<>$6 AND parent_session_id IS NULL END FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, result.SessionID, f.user, f.session, method, f.computer).Scan(&valid); err != nil || !valid {
			t.Fatalf("%s origin/isolation: %v %v", method, valid, err)
		}
	}
	helper := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'helper'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) SELECT environment_id,$2,deployment_id,template_id,has_init FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3`, pgx.QueryExecModeSimpleProtocol, f.env, helper, f.agent)
	req := f.startRequest("helper-denied")
	req.Agent = "helper"
	if _, err := session.Spawn(ctx, f.pool, nil, caller, req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("ungranted helper admitted: %v", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := session.Spawn(ctx, f.pool, nil, caller, f.startRequest("revoked")); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("revoked origin spawned: %v", err)
	}
}
