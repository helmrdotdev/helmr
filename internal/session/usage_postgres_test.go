package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func TestUsageSummarizesWorkPeopleAndRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	long, _ := f.finalize(t, "long")
	short, _ := f.finalize(t, "short")
	// Two hours and thirty seconds of work, admitted today.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET started_at=now()-interval '3 hours',terminal_at=now()-interval '1 hour' WHERE environment_id=$1 AND id=$2;
 UPDATE turns SET started_at=now()-interval '90 seconds',terminal_at=now()-interval '60 seconds' WHERE environment_id=$1 AND id=$3`, pgx.QueryExecModeSimpleProtocol, f.env, long.TurnID, short.TurnID)
	// A Turn from 40 days ago counts only in the previous 30-day window.
	old, _ := f.finalize(t, "old")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET created_at=now()-interval '40 days',started_at=now()-interval '40 days',terminal_at=now()-interval '40 days'+interval '10 minutes' WHERE environment_id=$1 AND id=$2`, f.env, old.TurnID)

	// The organization's headcount is shown to people who can read members.
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, org, f.user, nil, "members.read")

	u, err := session.ReadUsage(ctx, f.pool, f.caller(), f.env, 30)
	if err != nil {
		t.Fatal(err)
	}
	if u.Turns != 2 || u.PreviousTurns != 1 || u.WorkSeconds < 7229 || u.WorkSeconds > 7231 || u.PreviousWorkSeconds < 599 || u.PreviousWorkSeconds > 601 {
		t.Fatalf("work %+v", u)
	}
	if len(u.DailyWorkSeconds) != 30 || u.DailyWorkSeconds[29] < 7229 {
		t.Fatalf("daily work %v", u.DailyWorkSeconds)
	}
	if u.WeeklyPeople != 1 || u.AppPeople != 1 || u.SlackPeople != 0 || u.Members == nil || *u.Members < 1 || len(u.DailyWeeklyPeople) != 30 || u.DailyWeeklyPeople[29] != 1 {
		t.Fatalf("people %+v", u)
	}
	if u.RunBuckets != [5]int64{1, 0, 0, 1, 0} || u.Completed != 2 || u.Unassisted != 2 || u.Longest == nil || u.Longest.TurnID != long.TurnID || u.Longest.SessionID != f.session {
		t.Fatalf("runs %+v %+v", u.RunBuckets, u.Longest)
	}
	if len(u.Agents) != 1 || u.Agents[0].People != 1 || u.Agents[0].WorkSeconds < 7229 {
		t.Fatalf("agents %+v", u.Agents)
	}
	// A run that crosses the window's start is split between the windows: it
	// was admitted eight days ago and ran for two days.
	split, _ := f.finalize(t, "split")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET created_at=now()-interval '8 days',started_at=now()-interval '8 days',terminal_at=now()-interval '6 days' WHERE environment_id=$1 AND id=$2`, f.env, split.TurnID)
	week, err := session.ReadUsage(ctx, f.pool, f.caller(), f.env, 7)
	if err != nil {
		t.Fatal(err)
	}
	day := 86400.0
	if week.WorkSeconds < day+7229 || week.WorkSeconds > day+7232 || week.PreviousWorkSeconds < day || week.PreviousWorkSeconds > day+2 || week.DailyWorkSeconds[0] < day-1 {
		t.Fatalf("split run %v %v %v", week.WorkSeconds, week.PreviousWorkSeconds, week.DailyWorkSeconds)
	}
	if _, err := session.ReadUsage(ctx, f.pool, f.caller(), f.env, 14); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatal("unsupported range accepted", err)
	}
}

func TestUsageKeepsSessionReadBounds(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	peer := f.peer(t)
	otherAgent, viewer := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'other');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 UPDATE sessions SET agent_id=$2 WHERE id=$4;
 INSERT INTO users(id,display_name) VALUES($5,'Usage viewer');
 INSERT INTO org_members(id,org_id,user_id) SELECT uuidv7(),org_id,$5 FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, otherAgent, f.agent, peer.session, viewer)
	f.enqueue(t, "visible")
	peer.enqueue(t, "hidden")
	caller := session.Caller{Kind: "user", ID: viewer, CredentialID: uuid.NewV7()}
	if _, err := session.ReadUsage(ctx, f.pool, caller, f.env, 7); !errors.Is(err, session.ErrDenied) {
		t.Fatal("usage without session access", err)
	}
	var org uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	member := dbtest.GrantMember(t, f.pool, org, viewer, &f.env, "sessions.read")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE role_grants SET all_agents=false WHERE member_id=$1;
 INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE member_id=$1`, pgx.QueryExecModeSimpleProtocol, member, f.agent)
	u, err := session.ReadUsage(ctx, f.pool, caller, f.env, 7)
	if err != nil {
		t.Fatal(err)
	}
	if u.Turns != 1 || len(u.Agents) != 1 || u.Agents[0].Agent == "other" {
		t.Fatalf("usage escaped the selected Agent: %+v", u)
	}
	if u.Members != nil {
		t.Fatalf("headcount shown without members read: %d", *u.Members)
	}
}
