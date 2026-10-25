package computer_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerUsageCollectionsFilterBeforePagination(t *testing.T) {
	f := sessiontest.New(t)
	var scope computer.Scope
	scope.EnvironmentID = f.Environment
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	// A terminal Session with an unfenced process remains current usage.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Session)
	second := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id) VALUES($1,$2,$3,$4,$5,$2,0,(SELECT origin_user_id FROM sessions WHERE environment_id=$1 ORDER BY created_at,id LIMIT 1))`, f.Environment, second, f.Agent, f.Deployment, f.Computer)
	// Commands are newer than both Sessions: a combined page followed by filtering would lose Sessions.
	commandIDs := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	for _, id := range commandIDs {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO commands(environment_id,id,computer_id,computer_lease_epoch,status,argv,env,stdin,timeout_ms,caller_kind,caller_id,terminal_at,terminal_reason_code) VALUES($1,$2,$3,1,'cancelled',ARRAY['true'],'{}',decode('','hex'),1000,'user',$4,clock_timestamp(),'cancelled')`, f.Environment, id, f.Computer, f.User)
	}
	for _, kind := range []string{"session", "command"} {
		seen := map[string]bool{}
		cursor := ""
		want := 2
		if kind == "command" {
			want = 3
		}
		for i := 0; i < want; i++ {
			page, err := computer.ListMembers(t.Context(), f.Pool, scope, f.Computer, computer.MembersQuery{Kind: kind, Limit: 1, Cursor: cursor})
			if err != nil || len(page.Members) != 1 {
				t.Fatalf("%s page %d: %+v %v", kind, i, page, err)
			}
			item := page.Members[0]
			if item.Kind != kind || seen[item.ID] {
				t.Fatalf("unexpected member: %+v", item)
			}
			seen[item.ID] = true
			if (kind == "command" || item.ID == f.Session.String()) && item.State != "unreconciled" {
				t.Fatalf("cleanup omitted: %+v", item)
			}
			cursor = page.NextCursor
			if (cursor != "") != (i < want-1) {
				t.Fatalf("%s page %d cursor=%q", kind, i, cursor)
			}
			if cursor != "" {
				otherKind := "session"
				if kind == "session" {
					otherKind = "command"
				}
				for _, bad := range []struct {
					env, id uuid.UUID
					kind    string
				}{{f.Environment, f.Computer, otherKind}, {uuid.NewV7(), f.Computer, kind}, {f.Environment, uuid.NewV7(), kind}} {
					if err := computer.ValidateMembersQuery(bad.env, bad.id, computer.MembersQuery{Kind: bad.kind, Cursor: cursor}); err == nil {
						t.Fatal("cursor escaped collection or scope")
					}
				}
			}
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE commands SET process_reconciled_at=clock_timestamp() WHERE environment_id=$1`, f.Environment)
	page, err := computer.ListMembers(t.Context(), f.Pool, scope, f.Computer, computer.MembersQuery{Kind: "command"})
	if err != nil || len(page.Members) != 0 {
		t.Fatalf("reconciled history shown as usage: %+v %v", page, err)
	}
}
