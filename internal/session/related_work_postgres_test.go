package session_test

import (
	"encoding/json"
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
		page, err := session.ListSessions(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
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
	page, err := session.ListSessions(t.Context(), policy, f.pool, f.caller(), session.SessionListRequest{EnvironmentID: f.env, RelatedUserID: contributor, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != f.session || page.Sessions[0].RelatedVia != "answered" {
		t.Fatalf("answer contribution: %+v %v", page, err)
	}
}
