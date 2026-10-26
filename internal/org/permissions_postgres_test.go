package org_test

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestScopedRoleGrantsAndSharedEdits(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	m := f.owner
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO regions(id,display_name) VALUES('permissions','Permissions')`)
	project, environments, err := org.CreateProject(ctx, f.pool, m, org.ProjectInput{ProjectDetails: org.ProjectDetails{Slug: "permission-project", Name: "Permissions"}, ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	env := pgvalue.MustUUIDValue(environments[0].ID)
	otherEnv := pgvalue.MustUUIDValue(environments[1].ID)
	agent, otherAgent := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'invoice'),($3,$2,'faq')`, agent, env, otherAgent)
	if err = db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
		return org.SeedStandardRoles(ctx, tx, auditpolicy.Policy{RetentionSeconds: 3600}, m.OrgID, m.Actor)
	}); err != nil {
		t.Fatal(err)
	}
	roles, err := org.ListPermissionRoles(ctx, f.pool, m)
	if err != nil || len(roles) != 17 {
		t.Fatalf("standard roles=%v %v", roles, err)
	}
	if err = db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
		return org.SeedStandardRoles(ctx, tx, auditpolicy.Policy{RetentionSeconds: 3600}, m.OrgID, m.Actor)
	}); err != nil {
		t.Fatal(err)
	}
	group, err := org.SavePermissionGroup(ctx, f.pool, m, uuid.Nil(), "Business team")
	if err != nil {
		t.Fatal(err)
	}
	if err = org.SetPermissionGroupMember(ctx, f.pool, m, group.ID, f.member.MemberID, true); err != nil {
		t.Fatal(err)
	}
	role, err := org.SavePermissionRole(ctx, f.pool, m, uuid.Nil(), org.RoleInput{Name: "Invoice users", Permissions: []string{"agents.use"}})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := org.AssignRole(ctx, f.pool, m, org.GrantInput{ProjectID: &projectID, Subject: org.GrantSubject{Type: "group", GroupID: &group.ID}, RoleID: role.ID, EnvironmentID: &env, AgentsMode: "selected", AgentIDs: []uuid.UUID{agent}, ExpectedRoleRevision: role.Revision})
	if err != nil {
		t.Fatal(err)
	}
	check := func(agent, environment uuid.UUID, p auth.Permission, want bool) {
		t.Helper()
		err := access.Check(ctx, f.pool, "user", f.member.UserID, environment, agent, p)
		if want && err != nil || !want && !errors.Is(err, access.ErrDenied) {
			t.Fatalf("permission %s agent %s allowed=%t err=%v", p, agent, want, err)
		}
	}
	check(agent, env, auth.PermissionAgentsUse, true)
	check(otherAgent, env, auth.PermissionAgentsUse, false)
	check(agent, otherEnv, auth.PermissionAgentsUse, false)
	check(agent, env, auth.PermissionSessionsRead, false)
	if _, err = org.SavePermissionRole(ctx, f.pool, m, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"assignments.create"}}); !errors.Is(err, org.ErrRoleTarget) {
		t.Fatalf("incompatible edit: %v", err)
	}
	check(agent, env, auth.PermissionSessionsRead, false)
	if err = org.DeletePermissionRole(ctx, f.pool, m, role.ID); !errors.Is(err, org.ErrRoleInUse) {
		t.Fatalf("assigned role deletion: %v", err)
	}
	// An independent All Users assignment survives removing one group path.
	if _, err = org.AssignRole(ctx, f.pool, m, org.GrantInput{ProjectID: &projectID, Subject: org.GrantSubject{Type: "all_members"}, RoleID: role.ID, EnvironmentID: &env, AgentsMode: "selected", AgentIDs: []uuid.UUID{agent}, ExpectedRoleRevision: role.Revision}); err != nil {
		t.Fatal(err)
	}
	if err = org.RemoveRoleGrant(ctx, f.pool, m, grants[0].ID, strconv.FormatInt(grants[0].Revision, 10)); err != nil {
		t.Fatal(err)
	}
	check(agent, env, auth.PermissionAgentsUse, true)
	if _, err = org.AssignRole(ctx, f.pool, m, org.GrantInput{ProjectID: &projectID, Subject: org.GrantSubject{Type: "all_members"}, RoleID: role.ID, EnvironmentID: &env, AgentsMode: "all", ExpectedRoleRevision: role.Revision}); err == nil {
		t.Fatal("All Users environment assignment accepted")
	}
	if err = identity.RemoveMember(ctx, f.pool, m, f.member.MemberID); err != nil {
		t.Fatal(err)
	}
	check(agent, env, auth.PermissionAgentsUse, false)
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM permission_group_members WHERE org_id=$1 AND member_id=(SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 ORDER BY created_at DESC LIMIT 1)`, m.OrgID, f.member.UserID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("offboard group edges=%d %v", count, err)
	}
	for _, standard := range roles {
		if err = org.DeletePermissionRole(ctx, f.pool, m, standard.ID); !errors.Is(err, org.ErrStandardRole) {
			t.Fatalf("standard deletion: %v", err)
		}
	}
}

func TestRoleEditRacesAgentGrant(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO regions(id,display_name) VALUES('race','Race')`)
	project, environments, err := org.CreateProject(ctx, f.pool, f.owner, org.ProjectInput{ProjectDetails: org.ProjectDetails{Slug: "race", Name: "Race"}, ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	projectID, env := pgvalue.MustUUIDValue(project.ID), pgvalue.MustUUIDValue(environments[0].ID)
	agent := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'race')`, agent, env)
	for i := 0; i < 4; i++ {
		role, err := org.SavePermissionRole(ctx, f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: uuid.NewV7().String(), Permissions: []string{"agents.use"}})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := org.AssignRole(ctx, f.pool, f.admin, org.GrantInput{ProjectID: &projectID, Subject: org.GrantSubject{Type: "all_members"}, RoleID: role.ID, EnvironmentID: &env, AgentsMode: "selected", AgentIDs: []uuid.UUID{agent}, ExpectedRoleRevision: role.Revision})
			results <- err
		}()
		go func() {
			<-start
			_, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"sessions.read"}})
			results <- err
		}()
		close(start)
		successes := 0
		for j := 0; j < 2; j++ {
			err := <-results
			if err == nil {
				successes++
			} else if !errors.Is(err, org.ErrRoleTarget) && !errors.Is(err, org.ErrStaleRevision) {
				t.Fatal(err)
			}
		}
		if successes != 1 {
			t.Fatalf("successful incompatible writers: %d", successes)
		}
		var incompatible bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_grants g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id) CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.role_id=$1 AND NOT g.all_agents AND p.permission<>'agents.use')`, role.ID).Scan(&incompatible); err != nil || incompatible {
			t.Fatal("incompatible grant committed", incompatible, err)
		}
		var facts int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE (action='role.updated' AND target_id=$1) OR (action='role_grant.created' AND details->'after'->>'role_id'=$1::text)`, role.ID).Scan(&facts); err != nil || facts != 1 {
			t.Fatal("race audit mismatch", facts, err)
		}
	}
}

func TestOffboardRacesGroupAdd(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	group, err := org.SavePermissionGroup(ctx, f.pool, f.owner, uuid.Nil(), "Race group")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	added := make(chan error, 1)
	removed := make(chan error, 1)
	go func() {
		<-start
		added <- org.SetPermissionGroupMember(ctx, f.pool, f.admin, group.ID, f.member.MemberID, true)
	}()
	go func() { <-start; removed <- identity.RemoveMember(ctx, f.pool, f.owner, f.member.MemberID) }()
	close(start)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if err := <-added; err != nil && !errors.Is(err, org.ErrMemberNotFound) {
		t.Fatal(err)
	}
	var safe bool
	if err := f.pool.QueryRow(ctx, `SELECT removed_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM permission_group_members WHERE org_id=$1 AND member_id=(SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 ORDER BY created_at DESC LIMIT 1)) FROM org_members WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.member.UserID).Scan(&safe); err != nil || !safe {
		t.Fatal("offboard retained membership", safe, err)
	}
	if err := org.SetPermissionGroupMember(ctx, f.pool, f.admin, group.ID, f.member.MemberID, true); !errors.Is(err, org.ErrMemberNotFound) {
		t.Fatal("disabled member re-added", err)
	}
}

func TestRoleRenameAndGroupRemovalWithManyGrants(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	role, err := org.SavePermissionRole(ctx, f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: "Many grants", Permissions: []string{"members.read"}})
	if err != nil {
		t.Fatal(err)
	}
	group, err := org.SavePermissionGroup(ctx, f.pool, f.owner, uuid.Nil(), "Many assignments")
	if err != nil {
		t.Fatal(err)
	}
	// Distinct groups carry valid Organization grants without an artificial grant
	// count cap. Their aggregate snapshots exceed a single event's detail bound.
	for i := 0; i < 60; i++ {
		gid := uuid.NewV7()
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO permission_groups(id,org_id,name) VALUES($1,$2,$3)`, gid, f.owner.OrgID, fmt.Sprintf("Assignment group %d", i))
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO role_grants(id,org_id,group_id,role_id) VALUES($1,$2,$3,$4)`, uuid.NewV7(), f.owner.OrgID, gid, role.ID)
	}
	renamed, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: "Renamed", Permissions: role.Permissions, ExpectedRevision: strconv.FormatInt(role.Revision, 10)})
	if err != nil || renamed.Name != "Renamed" {
		t.Fatalf("many-grant rename: %+v %v", renamed, err)
	}
	// Group removal already records each grant; the parent must not repeat all IDs.
	for i := 0; i < 450; i++ {
		rid := uuid.NewV7()
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO roles(id,org_id,name,permissions) VALUES($1,$2,$3,ARRAY['members.read'])`, rid, f.owner.OrgID, fmt.Sprintf("Group role %d", i))
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO role_grants(id,org_id,group_id,role_id) VALUES($1,$2,$3,$4)`, uuid.NewV7(), f.owner.OrgID, group.ID, rid)
	}
	if err := org.DeletePermissionGroup(ctx, f.pool, f.owner, group.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='role_grant.deleted' AND details->'before'->>'group_id'=$1`, group.ID.String()).Scan(&count); err != nil || count != 450 {
		t.Fatalf("bounded grant facts: %d %v", count, err)
	}
}

func TestGrantAuditAtMaximumSelectedAgentSize(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO regions(id,display_name) VALUES('max-agent','Maximum')`)
	project, envs, err := org.CreateProject(ctx, f.pool, f.owner, org.ProjectInput{ProjectDetails: org.ProjectDetails{Slug: "max-agent", Name: "Maximum"}, ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	pid, env := pgvalue.MustUUIDValue(project.ID), pgvalue.MustUUIDValue(envs[0].ID)
	ids := make([]uuid.UUID, 100)
	for i := range ids {
		ids[i] = uuid.NewV7()
		dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,$3)`, ids[i], env, fmt.Sprintf("a%03d%s", i, strings.Repeat("a", 124)))
	}
	role, err := org.SavePermissionRole(ctx, f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: "Maximum agents", Permissions: []string{"agents.use"}})
	if err != nil {
		t.Fatal(err)
	}
	input := org.GrantInput{ProjectID: &pid, EnvironmentID: &env, Subject: org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}, RoleID: role.ID, ExpectedRoleRevision: role.Revision, AgentsMode: "selected", AgentIDs: ids}
	grants, err := org.AssignRole(ctx, f.pool, f.owner, input)
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = strconv.FormatInt(grants[0].Revision, 10)
	input.AgentIDs = ids[:99]
	if _, err := org.UpdateRoleGrant(ctx, f.pool, f.owner, grants[0].ID, input); err != nil {
		t.Fatal(err)
	}
	var bounded bool
	if err := f.pool.QueryRow(ctx, `SELECT bool_and(octet_length(details::text)<16384 AND NOT(details->'after' ? 'agent_keys')) FROM audit_events WHERE target_id=$1`, grants[0].ID).Scan(&bounded); err != nil || !bounded {
		t.Fatalf("grant evidence unbounded: %v %v", bounded, err)
	}
}
