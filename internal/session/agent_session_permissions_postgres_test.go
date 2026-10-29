package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func TestSessionPermissionsKeepSelectedAgentAndOriginBounds(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	peer := f.peer(t)
	otherAgent, viewer := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'other');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 UPDATE sessions SET agent_id=$2 WHERE id=$4;
 INSERT INTO users(id,display_name) VALUES($5,'Session viewer');
 INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$5 FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, otherAgent, f.agent, peer.session, viewer)
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	member := dbtest.GrantMember(t, f.pool, org, viewer, &f.env, "sessions.read")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id=$1;
 INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE member_id=$1`, pgx.QueryExecModeSimpleProtocol, member, f.agent)
	caller := session.UserCaller((sessiontest.Fixture{Pool: f.pool, User: viewer, Environment: f.env}).Principal(t))
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, f.session); err != nil {
		t.Fatal("selected personal history denied", err)
	}
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, peer.session); !errors.Is(err, session.ErrDenied) {
		t.Fatal("other Agent history exposed", err)
	}
	page, err := session.ListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionListRequest{EnvironmentID: f.env, Limit: 1})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session || page.NextCursor != uuid.Nil() {
		t.Fatalf("selected filter/pagination: %+v %v", page, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,uuidv7(),$2,'local','visible','user',$4),($1,uuidv7(),$3,'local','hidden','user',$4)`, f.env, f.session, peer.session, f.user)
	attention, err := session.ListAttention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env)
	if err != nil || attention.HoldCount != 1 || len(attention.Holds) != 1 || attention.Holds[0].SessionID != f.session {
		t.Fatalf("attention escaped: %+v %v", attention, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM session_holds WHERE environment_id=$1`, f.env)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE roles SET permissions=ARRAY['sessions.send','sessions.read','sessions.close'] WHERE id IN (SELECT role_id FROM role_grants WHERE member_id=$1)`, member)
	input := session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "management-input", Input: json.RawMessage(`[]`)}
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, input); !errors.Is(err, session.ErrDenied) {
		t.Fatal("management alone executed", err)
	}
	// A separate assignment combines only on the same Agent.
	dbtest.GrantMember(t, f.pool, org, viewer, &f.env, "agents.use")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id=$1 AND all_agents;
 INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT g.id,g.environment_id,$2 FROM role_grants g JOIN roles r ON r.id=g.role_id CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.member_id=$1 AND p.permission='agents.use'`, pgx.QueryExecModeSimpleProtocol, member, otherAgent)
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, input); !errors.Is(err, session.ErrDenied) {
		t.Fatal("other Agent use was borrowed", err)
	}
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT g.id,g.environment_id,$2 FROM role_grants g JOIN roles r ON r.id=g.role_id CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.member_id=$1 AND p.permission='agents.use'`, member, f.agent)
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, input); err != nil {
		t.Fatal("matching grants did not combine", err)
	}
	// Management cannot revive a personal runtime whose original owner lost use.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE roles SET permissions=array_remove(permissions,'agents.use') WHERE id IN (SELECT g.role_id FROM role_grants g JOIN permission_group_members gm ON gm.group_id=g.group_id JOIN org_members m ON m.id=gm.member_id WHERE m.user_id=$1)`, f.user)
	input.IdempotencyKey = "revoked-origin"
	if _, err := session.Enqueue(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, input); !errors.Is(err, session.ErrDenied) {
		t.Fatal("management revived origin", err)
	}
	if _, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, f.session); err != nil {
		t.Fatal("revoked origin hid retained history", err)
	}
	// Closing a parent cannot silently close a hidden child on another Agent.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET parent_session_id=$1,requester_session_id=$1,root_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, peer.session)
	err = db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := session.SessionCloseScope(ctx, tx, caller, f.env, f.session)
		return err
	})
	if !errors.Is(err, session.ErrDenied) {
		t.Fatal("hidden close scope exposed", err)
	}
	if _, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "close", IdempotencyKey: "cross-agent-close"}); !errors.Is(err, session.ErrDenied) {
		t.Fatal("cross-Agent cascade accepted", err)
	}
	// Metadata must not reveal hidden ancestors, even when their holds affect a
	// visible Session. Both direct reads and list rows use the same visibility.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET parent_session_id=NULL,requester_session_id=NULL,root_session_id=id,causal_depth=0 WHERE id=$1;
 UPDATE sessions SET parent_session_id=$1,requester_session_id=$1,root_session_id=$1,causal_depth=1 WHERE id=$2;
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($3,uuidv7(),$1,'subtree','hidden ancestor details','user',$4)`, pgx.QueryExecModeSimpleProtocol, peer.session, f.session, f.env, f.user)
	checkRelations := func(visible bool) {
		t.Helper()
		view, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env, f.session)
		if err != nil {
			t.Fatal(err)
		}
		page, err := session.ListSessions(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionListRequest{EnvironmentID: f.env, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		views := []session.SessionView{view}
		for _, row := range page.Sessions {
			if row.ID == f.session {
				views = append(views, row)
			}
		}
		if len(views) != 2 {
			t.Fatal("visible row missing", page)
		}
		for _, row := range views {
			if len(row.Holds) != 1 {
				t.Fatal("effective hold missing", row.Holds)
			}
			hold := row.Holds[0]
			if visible {
				if row.RootSessionID != peer.session || row.ParentSessionID == nil || *row.ParentSessionID != peer.session || row.RequesterSessionID == nil || *row.RequesterSessionID != peer.session || hold.SessionID != peer.session || hold.ID == uuid.Nil() || hold.Reason != "hidden ancestor details" {
					t.Fatalf("visible relations lost: %+v", row)
				}
			} else if row.RootSessionID != uuid.Nil() || row.ParentSessionID != nil || row.RequesterSessionID != nil || hold.SessionID != uuid.Nil() || hold.ID != uuid.Nil() || hold.Reason != "An ancestor session is held" || hold.RecoveryGap != nil {
				t.Fatalf("hidden relations exposed: %+v", row)
			}
		}
	}
	checkRelations(false)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT g.id,g.environment_id,$2 FROM role_grants g JOIN roles r ON r.id=g.role_id CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.member_id=$1 AND p.permission='sessions.send'`, member, otherAgent)
	checkRelations(true)

}
