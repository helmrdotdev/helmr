package org

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestResourceCreatorsReceiveExplicitAccessWithoutAssignmentPermission(t *testing.T) {
	f := newProjectFixture(t, 1, 1)
	ctx := t.Context()
	// Resource creation alone is sufficient, without ownership or any access
	// administration. Existing resources must remain outside the new grants.
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grants WHERE org_id=$1 AND member_id=$2`, f.orgID, f.manager.MemberID)
	dbtest.GrantMember(t, f.pool, f.orgID, f.manager.UserID, nil, "projects.create", "environments.create")
	before, err := f.manager.Current(ctx, f.pool)
	if err != nil || before.IsOwner || before.HasPermission(auth.PermissionAssignmentsCreate, auth.Scope{OrgID: f.orgID}) {
		t.Fatalf("creator unexpectedly has access administration: %+v %v", before, err)
	}
	project, environments, err := CreateProject(ctx, f.pool, f.manager, ProjectInput{ExecutionLimits: testExecutionLimits(), Slug: "created"})
	if err != nil || len(environments) != 2 {
		t.Fatalf("create project: %d %v", len(environments), err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	extra, err := CreateEnvironment(ctx, f.pool, f.manager, projectID, EnvironmentDetails{Slug: "extra", ColorHex: "#3366ff"}, testExecutionLimits())
	if err != nil {
		t.Fatal(err)
	}
	environments = append(environments, extra)
	current, err := f.manager.Current(ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range environments {
		scope := auth.Scope{OrgID: f.orgID, ProjectID: projectID.String(), EnvironmentID: pgvalue.MustUUIDValue(environment.ID).String(), AgentID: uuid.NewV7().String()}
		for _, role := range auth.StandardRoles() {
			if role.Key != "environment_resource_administrator" && role.Key != "auditor" {
				continue
			}
			for _, permission := range role.Permissions {
				if !current.HasPermission(permission, scope) {
					t.Fatalf("creator lacks %s in %s", permission, environment.Slug)
				}
			}
		}
	}
	if current.HasPermission(auth.PermissionAuditEventsRead, auth.Scope{OrgID: f.orgID}) || current.HasPermission(auth.PermissionAssignmentsCreate, auth.Scope{OrgID: f.orgID}) {
		t.Fatal("environment creation granted organization administration")
	}
	var existingEnv uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 LIMIT 1`, f.projectIDs[0]).Scan(&existingEnv); err != nil {
		t.Fatal(err)
	}
	if current.HasPermission(auth.PermissionSecretsCreate, auth.Scope{OrgID: f.orgID, ProjectID: f.projectIDs[0].String(), EnvironmentID: existingEnv.String()}) {
		t.Fatal("creator received access in a pre-existing environment")
	}
	if current.HasPermission(auth.PermissionSecretsCreate, auth.Scope{OrgID: uuid.NewV7(), ProjectID: projectID.String(), EnvironmentID: pgvalue.MustUUIDValue(extra.ID).String()}) {
		t.Fatal("creator grant crosses organizations")
	}
	other := projectManager(t, f.pool, f.orgID)
	outsider, err := other.Current(ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if outsider.HasPermission(auth.PermissionSecretsCreate, auth.Scope{OrgID: f.orgID, ProjectID: projectID.String(), EnvironmentID: pgvalue.MustUUIDValue(extra.ID).String()}) {
		t.Fatal("creation granted access to another member")
	}
	var audited int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id=$1 AND project_id=$2 AND action='role_grant.created' AND actor_user_id=$3 AND details->'after'->>'member_id'=$4 AND details->'after'->>'target_kind'='environment' AND authorization_evidence->>'decision'='allowed' AND recorded_at > (SELECT created.recorded_at FROM audit_events created WHERE created.environment_id=audit_events.environment_id AND created.action='environment.created')`, f.orgID, projectID, f.manager.UserID, f.manager.MemberID.String()).Scan(&audited); err != nil || audited != 6 {
		t.Fatalf("creator assignment audit: %d %v", audited, err)
	}
	// Revoke through the ordinary access writer. Later creation must neither
	// resurrect the old grant nor convert creation authority into a bypass.
	dbtest.GrantMember(t, f.pool, f.orgID, f.manager.UserID, nil, "assignments.delete")
	var grantID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT g.id FROM role_grants g JOIN roles r ON r.id=g.role_id WHERE g.member_id=$1 AND g.environment_id=$2 AND r.builtin_key='environment_resource_administrator'`, f.manager.MemberID, extra.ID).Scan(&grantID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoleGrant(ctx, f.pool, f.manager, grantID, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateEnvironment(ctx, f.pool, f.manager, projectID, EnvironmentDetails{Slug: "later", ColorHex: "#3366ff"}, testExecutionLimits()); err != nil {
		t.Fatal(err)
	}
	current, err = f.manager.Current(ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if current.HasPermission(auth.PermissionSecretsCreate, auth.Scope{OrgID: f.orgID, ProjectID: projectID.String(), EnvironmentID: pgvalue.MustUUIDValue(extra.ID).String()}) {
		t.Fatal("revoked creator access was restored")
	}
}

func TestCreatorAccessFailureRollsBackResourceCreation(t *testing.T) {
	f := newProjectFixture(t, 1, 1)
	ctx := t.Context()
	// Fail the second assignment, after the first grant and audit were written.
	dbtest.MustExec(t, ctx, f.pool, `CREATE FUNCTION reject_creator_auditor() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF EXISTS(SELECT 1 FROM roles WHERE id=NEW.role_id AND builtin_key='auditor') THEN RAISE EXCEPTION 'injected creator assignment failure'; END IF; RETURN NEW; END $$;
    CREATE TRIGGER reject_creator_auditor BEFORE INSERT ON role_grants FOR EACH ROW EXECUTE FUNCTION reject_creator_auditor()`)
	if _, _, err := CreateProject(ctx, f.pool, f.manager, ProjectInput{ExecutionLimits: testExecutionLimits(), Slug: "failed"}); err == nil || !strings.Contains(err.Error(), "injected creator assignment failure") {
		t.Fatalf("project did not fail on creator assignment: %v", err)
	}
	if _, err := CreateEnvironment(ctx, f.pool, f.manager, f.projectIDs[0], EnvironmentDetails{Slug: "failed", ColorHex: "#3366ff"}, testExecutionLimits()); err == nil || !strings.Contains(err.Error(), "injected creator assignment failure") {
		t.Fatalf("environment did not fail on creator assignment: %v", err)
	}
	var intact bool
	if err := f.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM projects WHERE slug='failed') AND NOT EXISTS(SELECT 1 FROM environments WHERE slug='failed') AND NOT EXISTS(SELECT 1 FROM role_grants WHERE environment_id IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM audit_events WHERE action='role_grant.created')`).Scan(&intact); err != nil || !intact {
		t.Fatalf("partial creation committed: %v %v", intact, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `DROP TRIGGER reject_creator_auditor ON role_grants; DROP FUNCTION reject_creator_auditor()`)
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grants WHERE org_id=$1 AND member_id=$2`, f.orgID, f.manager.MemberID)
	if _, err := CreateEnvironment(ctx, f.pool, f.manager, f.projectIDs[0], EnvironmentDetails{Slug: "denied", ColorHex: "#3366ff"}, testExecutionLimits()); !errors.Is(err, ErrMemberManagementRequired) {
		t.Fatalf("creation without authority: %v", err)
	}
	if _, _, err := CreateProject(ctx, f.pool, f.manager, ProjectInput{ExecutionLimits: testExecutionLimits(), Slug: "denied"}); !errors.Is(err, ErrMemberManagementRequired) {
		t.Fatalf("project without authority: %v", err)
	}
}
