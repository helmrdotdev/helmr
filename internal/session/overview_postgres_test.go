package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func TestOverviewSummarizesVisibleWork(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.finalize(t, "completed")
	f.enqueue(t, "queued")
	// Two failures of one kind and one of another, all on the fixture Session.
	failed := []uuid.UUID{f.enqueue(t, "fail-1").TurnID, f.enqueue(t, "fail-2").TurnID, f.enqueue(t, "fail-3").TurnID}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='failed',error_code=CASE WHEN id=$4 THEN 'timeout' ELSE 'tool_error' END,error_message='\x00'::bytea,terminal_at=clock_timestamp() WHERE environment_id=$1 AND id=ANY(ARRAY[$2,$3,$4]::uuid[])`, f.env, failed[0], failed[1], failed[2])
	// A Turn from two days ago counts only in the previous window's total.
	old := f.enqueue(t, "old").TurnID
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='failed',error_code='old',error_message='\x00'::bytea,created_at=now()-interval '30 hours',terminal_at=now()-interval '30 hours' WHERE environment_id=$1 AND id=$2`, f.env, old)

	// A Turn admitted after the read began lands in the latest bucket.
	future := f.enqueue(t, "future").TurnID
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET created_at=clock_timestamp()+interval '5 minutes' WHERE environment_id=$1 AND id=$2`, f.env, future)

	o, err := session.ReadOverview(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	if o.Turns != 6 || o.Failed != 3 || o.PreviousTurns != 1 || o.PreviousFailed != 1 {
		t.Fatalf("totals %+v", o)
	}
	if o.ActiveCount != 1 || o.Queued != 1 || len(o.Active) != 1 || o.Active[0].ID != f.session || o.Active[0].State != "queued" || o.Active[0].Origin != "user" {
		t.Fatalf("active %+v", o.Active)
	}
	if len(o.Agents) != 1 || o.Agents[0].Turns != 6 || o.Agents[0].Failed != 3 || o.Agents[0].Active != 1 || o.Agents[0].Hourly[23] != 6 || o.Agents[0].HourlyFailed[23] != 3 {
		t.Fatalf("agents %+v", o.Agents)
	}
	if len(o.Failures) != 2 || o.Failures[0].ErrorCode != "tool_error" || o.Failures[0].Count != 2 || o.Failures[0].LastHour != 2 || o.Failures[1].ErrorCode != "timeout" {
		t.Fatalf("failures %+v", o.Failures)
	}

	// A subtree hold on the parent holds an idle child too, and outranks
	// queued work; counts cover every Session, not only those listed.
	child := f.requestedPeer(t)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET parent_session_id=$2,root_session_id=$2 WHERE environment_id=$1 AND id=$3;
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,uuidv7(),$2,'subtree','paused','user',$4)`, pgx.QueryExecModeSimpleProtocol, f.env, f.session, child.session, f.user)
	if o, err = session.ReadOverview(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env); err != nil || o.ActiveCount != 2 || o.Held != 2 || len(o.Active) != 2 || o.Active[0].State != "held" || o.Active[1].State != "held" || o.Agents[0].Active != 2 {
		t.Fatalf("held %+v %v", o, err)
	}
}

func TestOverviewKeepsSessionReadBounds(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	peer := f.peer(t)
	otherAgent, viewer := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'other');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 UPDATE sessions SET agent_id=$2 WHERE id=$4;
 INSERT INTO users(id,display_name) VALUES($5,'Overview viewer');
 INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$5 FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, otherAgent, f.agent, peer.session, viewer)
	f.enqueue(t, "visible")
	peer.enqueue(t, "hidden")
	caller := session.Caller{Kind: "user", ID: viewer, CredentialID: uuid.NewV7()}
	if _, err := session.ReadOverview(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env); !errors.Is(err, session.ErrDenied) {
		t.Fatal("overview without session access", err)
	}
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	member := dbtest.GrantMember(t, f.pool, org, viewer, &f.env, "sessions.read")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id=$1;
 INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE member_id=$1`, pgx.QueryExecModeSimpleProtocol, member, f.agent)
	o, err := session.ReadOverview(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if o.Turns != 1 || o.ActiveCount != 1 || o.Active[0].ID != f.session || len(o.Agents) != 1 || o.Agents[0].Agent == "other" {
		t.Fatalf("overview escaped the selected Agent: %+v", o)
	}
}

func TestOverviewCapturesFailureContentBeforeDisclosure(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	const code = "private application failure payload"
	turn := f.enqueue(t, "failure-content").TurnID
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='failed',error_code=$3,error_message='\x00'::bytea,terminal_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, turn, code)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	o, err := session.ReadOverview(ctx, policy, f.pool, f.caller(), f.env)
	if err != nil || len(o.Failures) != 1 || o.Failures[0].ErrorCode != code {
		t.Fatalf("failure content: %+v %v", o, err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='session.content_read' AND session_id=$1 AND turn_id=$2`, f.session, turn).Scan(&count); err != nil || count != 1 {
		t.Fatalf("disclosure fact count=%d error=%v", count, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `CREATE FUNCTION reject_overview_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER reject_overview_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_overview_audit()`)
	o, err = session.ReadOverview(ctx, policy, f.pool, f.caller(), f.env)
	if !errors.Is(err, audit.ErrCapture) || len(o.Failures) != 0 {
		t.Fatalf("unaudited disclosure: %+v %v", o, err)
	}
}
