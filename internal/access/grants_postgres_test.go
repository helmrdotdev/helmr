package access_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestGrantReadersPreserveTargetsAndCallerTransaction(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'reader','reader-prefix',$5)`, key, org, project, f.Environment, dbtest.Hash("reader-key"))
	// The fixture has a human Group grant; it must not reach an unassigned key.
	if grants, err := access.APIKeyGrants(ctx, f.Pool, org, key); err != nil || len(grants) != 0 {
		t.Fatalf("unassigned key: %v %v", grants, err)
	}
	dbtest.SetAPIKeyRole(t, f.Pool, key, "agents.use", "sessions.view", "sessions.manage", "secrets.manage")
	read := func(database db.DBTX, wantAgent bool, target string) {
		t.Helper()
		human, err := access.MemberGrants(ctx, database, org, f.MemberID)
		if err != nil {
			t.Fatal(err)
		}
		machine, err := access.APIKeyGrants(ctx, database, org, key)
		if err != nil {
			t.Fatal(err)
		}
		grants := make([]auth.PermissionGrant, 0, len(human))
		for _, g := range human {
			grants = append(grants, g.PermissionGrant)
			if g.ProjectSlug != "project" || g.EnvironmentSlug != "test" || g.AgentID != "" && g.AgentKey != "agent" {
				t.Fatalf("display names lost: %+v", g)
			}
		}
		for _, subject := range []struct {
			name   string
			grants []auth.PermissionGrant
		}{{"human", grants}, {"key", machine}} {
			foundAgent, foundSecret := false, false
			for _, g := range subject.grants {
				if auth.CapabilityResourceType(g.Permission) == "agents" {
					foundAgent = true
					if g.AgentID != target {
						t.Fatalf("%s target widened: %+v", subject.name, g)
					}
				}
				if g.Permission == auth.CapabilitySecretsManage {
					foundSecret = true
					if g.AgentID != "" {
						t.Fatalf("non-Agent permission narrowed: %+v", g)
					}
				}
			}
			if foundAgent != wantAgent || !foundSecret {
				t.Fatalf("%s targets: %+v", subject.name, subject.grants)
			}
		}
	}
	read(f.Pool, true, "")
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE role_grants SET agents_mode='selected' WHERE environment_id=$1`, f.Environment)
	read(f.Pool, false, "")
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1`, f.Environment, f.Agent)
	read(f.Pool, true, f.Agent.String())
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Uncommitted policy is visible through the supplied transaction, not the pool.
	dbtest.MustExec(t, ctx, tx, `DELETE FROM role_grant_agents WHERE environment_id=$1`, f.Environment)
	read(tx, false, "")
	read(f.Pool, true, f.Agent.String())
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM role_grant_agents WHERE environment_id=$1`, f.Environment)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE role_grants SET agents_mode='none' WHERE environment_id=$1`, f.Environment)
	read(f.Pool, false, "")
	// Live Role edits remove non-Agent authority as well.
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM role_permissions WHERE org_id=$1 AND permission='secrets.manage'`, org)
	grants, err := access.APIKeyGrants(ctx, f.Pool, org, key)
	if err != nil || len(grants) != 0 {
		t.Fatalf("stale Role: %v %v", grants, err)
	}
	dbtest.SetAPIKeyRole(t, f.Pool, key, "secrets.manage")
	for _, state := range []string{"expired", "revoked"} {
		dbtest.MustExec(t, ctx, f.Pool, `UPDATE api_keys SET expires_at=CASE WHEN $2='expired' THEN now()-interval '1 minute' END,revoked_at=CASE WHEN $2='revoked' THEN now() END WHERE id=$1`, key, state)
		if grants, err := access.APIKeyGrants(ctx, f.Pool, org, key); err != nil || len(grants) != 0 {
			t.Fatalf("%s key: %v %v", state, grants, err)
		}
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE org_members SET removed_at=now() WHERE id=$1`, f.MemberID)
	if grants, err := access.MemberGrants(ctx, f.Pool, org, f.MemberID); err != nil || len(grants) != 0 {
		t.Fatalf("removed membership: %v %v", grants, err)
	}
}
