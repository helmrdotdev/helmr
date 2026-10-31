package session_test

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"
)

func TestRelatedWorkFiltersBeforePaginationAndRetainsCurrentVisibility(t *testing.T) {
	f := newFixture(t)
	contributor := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Previous contributor')`, contributor)
	contribution := f.peer(t)
	turn := contribution.enqueue(t, "contribution")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET caller_id=$2 WHERE id=$1`, turn.TurnID, contributor)
	descendant := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,requester_session_id=$1,root_session_id=$1,causal_depth=1 WHERE id=$2`, contribution.session, descendant.session)
	hidden := f.peer(t)
	otherAgent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'hidden'); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3; UPDATE sessions SET origin_user_id=$4,agent_id=$2 WHERE id=$5`, pgx.QueryExecModeSimpleProtocol, f.env, otherAgent, f.agent, contributor, hidden.session)
	scopeFixtureToAgent(t, f)
	req := session.SessionListRequest{EnvironmentID: f.env, RelatedUserID: contributor, Limit: 1}
	seen := map[uuid.UUID]string{}
	for {
		page, err := session.ListSessions(t.Context(), f.pool, f.caller(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Sessions {
			if _, ok := seen[s.ID]; ok {
				t.Fatal("duplicate", s.ID)
			}
			seen[s.ID] = s.RelatedVia
		}
		if page.NextCursor == uuid.Nil() {
			break
		}
		req.Before = page.NextCursor
	}
	if len(seen) != 2 || seen[contribution.session] != "contributed" || seen[descendant.session] != "descendant" {
		t.Fatalf("visibility or attribution: %+v", seen)
	}
}

func TestRelatedWorkIncludesRetainedAnswers(t *testing.T) {
	f, a, ask, _ := askFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	if _, err := session.AnswerAsk(t.Context(), policy, f.pool, f.caller(), session.AskAnswerRequest{EnvironmentID: f.env, SessionID: f.session, TurnID: a.TurnID, AskID: ask, AnswerID: "historical-answer", Answer: json.RawMessage(`"approved"`)}); err != nil {
		t.Fatal(err)
	}
	contributor := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Former approver'); UPDATE turn_asks SET answered_by_user_id=$1,question=NULL,answer=NULL,payload_expired_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, contributor, ask)
	page, err := session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, RelatedUserID: contributor, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session || page.Sessions[0].RelatedVia != "answered" {
		t.Fatalf("answer contribution: %+v %v", page, err)
	}
}

func TestRelatedWorkOutlivesAdmissionAuditAndCredentialRecords(t *testing.T) {
	f := newFixture(t)
	caller := f.caller()
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 1}, f.pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "retained-attribution", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	// Wait for the admission event's actual expiry, not an assumed scheduler delay.
	dbtest.MustExec(t, t.Context(), f.pool, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM max(expires_at)-clock_timestamp()))+0.01) FROM audit_events WHERE session_id=$1 AND retention_seconds=1`, f.session)
	if _, err := audit.Purge(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 1}, 1000); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE session_id=$1 AND retention_seconds=1`, f.session).Scan(&retained); err != nil || retained != 0 {
		t.Fatal(retained, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM login_sessions WHERE id=$1; DELETE FROM auth_identities WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, caller.Human.SessionID, caller.Human.IdentityID)
	f.human = dbtest.BrowserPrincipal(t, f.pool, caller.Human.OrgID, f.user)
	page, err := session.ListSessions(t.Context(), f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, RelatedUserID: f.user, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session || page.Sessions[0].RelatedVia != "initiated" {
		t.Fatal("lost business attribution", page, err)
	}
}
