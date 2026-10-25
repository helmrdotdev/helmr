package access_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

type beforeGrantRead struct {
	db.DBTX
	before func()
}

func (d beforeGrantRead) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	d.before()
	return d.DBTX.Query(ctx, query, args...)
}

func TestRemovedMembershipCannotAcquireLaterAudienceGrant(t *testing.T) {
	f := sessiontest.New(t)
	original := f.Principal(t)
	database := beforeGrantRead{DBTX: f.Pool, before: func() {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET removed_at=now() WHERE id=$1`, f.MemberID)
		f.SetPermissions(t, "agents.use")
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE role_grants SET group_id=NULL,all_members=true,all_agents=false WHERE environment_id=$1; INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Agent)
	}}
	p, err := access.OrganizationPrincipal(t.Context(), database, original.OrgID, f.User)
	if err != nil {
		t.Fatal(err)
	}
	scope := auth.Scope{OrgID: original.OrgID, ProjectID: original.ProjectID, EnvironmentID: original.EnvironmentID, AgentID: f.Agent.String()}
	if p.HasPermission(auth.PermissionAgentsUse, scope) || len(p.Grants) != 0 {
		t.Fatalf("removed membership acquired later grant: %+v", p.Grants)
	}
}
