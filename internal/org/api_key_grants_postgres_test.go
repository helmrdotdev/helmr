package org_test

import (
	"errors"
	"strconv"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
)

type machineGrantFixture struct {
	memberFixture
	project, env, otherEnv, key, agent, otherAgent uuid.UUID
}

func newMachineGrantFixture(t *testing.T) machineGrantFixture {
	t.Helper()
	f := newMemberFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO regions(id,display_name) VALUES('machines','Machines')`)
	project, envs, err := org.CreateProject(t.Context(), f.pool, f.owner, org.ProjectInput{Slug: "machines", Name: "Machines", ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	x := machineGrantFixture{memberFixture: f, project: pgvalue.MustUUIDValue(project.ID), env: pgvalue.MustUUIDValue(envs[0].ID), otherEnv: pgvalue.MustUUIDValue(envs[1].ID), key: uuid.NewV7(), agent: uuid.NewV7(), otherAgent: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'support'),($3,$2,'billing')`, x.agent, x.env, x.otherAgent)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,created_by_member_id,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,$5,'backend','backend',$6)`, x.key, f.owner.OrgID, x.project, x.env, f.owner.MemberID, dbtest.Hash(x.key.String()))
	return x
}
func (f machineGrantFixture) role(t *testing.T, permissions ...string) org.PermissionRole {
	t.Helper()
	r, err := org.SavePermissionRole(t.Context(), f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: uuid.NewV7().String(), Permissions: permissions})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f machineGrantFixture) input(role org.PermissionRole) org.GrantInput {
	return org.GrantInput{RoleID: role.ID, ExpectedRoleRevision: role.Revision, Subject: org.GrantSubject{Type: "api_key", APIKeyID: &f.key}, ProjectID: &f.project, EnvironmentID: &f.env, AgentsMode: "selected", AgentIDs: []uuid.UUID{f.agent}}
}
func (f machineGrantFixture) check(t *testing.T, key, agent, env uuid.UUID, permission auth.Permission, allowed bool) {
	t.Helper()
	err := access.Check(t.Context(), f.pool, "api_key", key, env, agent, permission)
	if allowed && err != nil || !allowed && !errors.Is(err, access.ErrDenied) {
		t.Fatalf("permission %s allowed=%t: %v", permission, allowed, err)
	}
}
func TestAPIKeyRoleGrantsUseCurrentSelectedTargets(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	role := f.role(t, "sessions.create")
	// A human sharing assignment cannot give an unassigned machine access.
	human := f.input(role)
	human.Subject = org.GrantSubject{Type: "all_members"}
	if _, err := org.AssignRole(ctx, f.pool, f.owner, human); err != nil {
		t.Fatal(err)
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsCreate, false)
	input := f.input(role)
	rows, err := org.AssignRole(ctx, f.pool, f.owner, input)
	if err != nil {
		t.Fatal(err)
	}
	grant := rows[0]
	if grant.APIKeyID == nil || *grant.APIKeyID != f.key || grant.APIKeyName != "backend" || grant.SubjectKind != "api_key" {
		t.Fatalf("subject projection: %+v", grant)
	}
	again, err := org.AssignRole(ctx, f.pool, f.owner, input)
	if err != nil || again[0].ID != grant.ID || again[0].Created {
		t.Fatal("duplicate assignment", err)
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsCreate, true)
	f.check(t, f.key, f.otherAgent, f.env, auth.PermissionSessionsCreate, false)
	f.check(t, f.key, f.agent, f.otherEnv, auth.PermissionSessionsCreate, false)
	// The same Role, assigned directly to a human, has the same operations/targets.
	human.Subject = org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}
	if _, err := org.AssignRole(ctx, f.pool, f.owner, human); err != nil {
		t.Fatal(err)
	}
	if err := access.Check(ctx, f.pool, "user", f.member.UserID, f.env, f.agent, auth.PermissionSessionsCreate); err != nil {
		t.Fatal(err)
	}
	// Remove All members before changing the shared Role to view+secrets.
	shared, err := org.QueryRoleGrants(ctx, f.pool, f.owner, org.GrantFilter{SubjectKind: "all_members"})
	if err != nil {
		t.Fatal(err)
	}
	if err := org.RemoveRoleGrant(ctx, f.pool, f.owner, shared[0].ID, strconv.FormatInt(shared[0].Revision, 10)); err != nil {
		t.Fatal(err)
	}
	updated, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"sessions.read", "secrets.create"}})
	if err != nil {
		t.Fatal(err)
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsCreate, false)
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsRead, true)
	f.check(t, f.key, f.otherAgent, f.env, auth.PermissionSessionsRead, false)
	f.check(t, f.key, f.otherAgent, f.env, auth.PermissionSecretsCreate, true)
	if err := access.Check(ctx, f.pool, "user", f.member.UserID, f.env, f.agent, auth.PermissionSessionsRead); err != nil {
		t.Fatal("shared edit did not reach human", err)
	}
	// Agent selection updates use the same optimistic revision and Role snapshot.
	input.ExpectedRoleRevision = updated.Revision
	input.ExpectedRevision = strconv.FormatInt(grant.Revision, 10)
	input.AgentIDs = []uuid.UUID{f.otherAgent}
	changed, err := org.UpdateRoleGrant(ctx, f.pool, f.owner, grant.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsRead, false)
	f.check(t, f.key, f.otherAgent, f.env, auth.PermissionSessionsRead, true)
	// Physical deletion is refused without changing the machine's target/revision.
	_, err = f.pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, f.otherAgent)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23001" {
		t.Fatalf("targeted Agent deletion: %v", err)
	}
	f.check(t, f.key, f.otherAgent, f.env, auth.PermissionSessionsRead, true)
	// Construct an empty selection explicitly to retain its fail-closed contract.
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grant_agents WHERE grant_id=$1`, grant.ID)
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsRead, false)
	current, err := org.QueryRoleGrants(ctx, f.pool, f.owner, org.GrantFilter{APIKeyID: f.key})
	if err != nil || len(current) != 1 || len(current[0].AgentIDs) != 0 || current[0].Revision != changed.Revision {
		t.Fatal("empty-target projection", current, err)
	}
	if err := org.RemoveRoleGrant(ctx, f.pool, f.owner, grant.ID, strconv.FormatInt(current[0].Revision, 10)); err != nil {
		t.Fatal(err)
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSecretsCreate, false)
}

func TestAPIKeyRoleCompatibilityAndScopeConstraints(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	role := f.role(t, "sessions.create")
	input := f.input(role)
	for _, permission := range []string{"api_keys.create", "agents.delete", "assignments.create", "projects.create"} {
		incompatible := f.role(t, permission)
		candidate := f.input(incompatible)
		if permission == "api_keys.create" {
			candidate.AgentsMode = "none"
			candidate.AgentIDs = nil
		}
		if _, err := org.AssignRole(ctx, f.pool, f.owner, candidate); !errors.Is(err, org.ErrRoleTarget) {
			t.Fatalf("assigned %s: %v", permission, err)
		}
	}
	if _, err := org.AssignRole(ctx, f.pool, f.owner, input); err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{"api_keys.create", "agents.delete"} {
		if _, err := org.SavePermissionRole(ctx, f.pool, f.owner, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"sessions.create", permission}}); !errors.Is(err, org.ErrRoleTarget) {
			t.Fatalf("edited incompatible Role %s: %v", permission, err)
		}
	}
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsCreate, true)
	bad := input
	bad.EnvironmentID = &f.otherEnv
	bad.AgentsMode = "all"
	bad.AgentIDs = nil
	if _, err := org.AssignRole(ctx, f.pool, f.owner, bad); !errors.Is(err, org.ErrAccessNotFound) {
		t.Fatal("cross-environment key assigned", err)
	}
	// Composite FK and exactly-one-subject checks stand independently of application validation.
	for _, tc := range []struct {
		name   string
		env    *uuid.UUID
		member *uuid.UUID
		code   string
	}{
		{"missing environment", nil, nil, "23514"},
		{"other environment", &f.otherEnv, nil, "23503"},
		{"two subjects", &f.env, &f.member.MemberID, "23514"},
	} {
		_, err := f.pool.Exec(ctx, `INSERT INTO role_grants(id,org_id,role_id,api_key_id,member_id,environment_id) VALUES($1,$2,$3,$4,$5,$6)`, uuid.NewV7(), f.owner.OrgID, role.ID, f.key, tc.member, tc.env)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != tc.code {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE api_keys SET revoked_at=now() WHERE id=$1`, f.key)
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsCreate, false)
	if _, err := org.AssignRole(ctx, f.pool, f.owner, input); !errors.Is(err, org.ErrAccessNotFound) {
		t.Fatal("assigned revoked key", err)
	}
}

func TestAPIKeyRoleAssignmentRacesIncompatibleRoleEdit(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	for range 4 {
		role := f.role(t, "sessions.create")
		input := f.input(role)
		start := make(chan struct{})
		assigned := make(chan error, 1)
		edited := make(chan error, 1)
		go func() { <-start; _, err := org.AssignRole(ctx, f.pool, f.owner, input); assigned <- err }()
		go func() {
			<-start
			_, err := org.SavePermissionRole(ctx, f.pool, f.admin, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: "1", Permissions: []string{"sessions.create", "api_keys.create"}})
			edited <- err
		}()
		close(start)
		assignErr, editErr := <-assigned, <-edited
		if assignErr == nil && editErr == nil {
			t.Fatal("incompatible Role and machine assignment both committed")
		}
		if assignErr != nil && !errors.Is(assignErr, org.ErrStaleRevision) && !errors.Is(assignErr, org.ErrRoleTarget) {
			t.Fatal(assignErr)
		}
		if editErr != nil && !errors.Is(editErr, org.ErrRoleTarget) {
			t.Fatal(editErr)
		}
		var bad bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_grants g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id) CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.api_key_id=$1 AND p.permission='api_keys.create')`, f.key).Scan(&bad); err != nil || bad {
			t.Fatal("persisted forbidden machine grant", err)
		}
	}
}

func TestEveryActionAPIKeyAssignmentEligibility(t *testing.T) {
	f := newMachineGrantFixture(t)
	for _, definition := range auth.PermissionCatalog() {
		t.Run(string(definition.ID), func(t *testing.T) {
			role := f.role(t, string(definition.ID))
			input := f.input(role)
			if !auth.SupportsScope(definition.ID, auth.ScopeAgent) {
				input.AgentsMode = "none"
				input.AgentIDs = nil
			}
			rows, err := org.AssignRole(t.Context(), f.pool, f.owner, input)
			if !definition.APIKeyAssignable {
				if !errors.Is(err, org.ErrRoleTarget) {
					t.Fatalf("human-only assignment: %v", err)
				}
				return
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("assignable action: %v", err)
			}
			scope := auth.Scope{OrgID: f.owner.OrgID, ProjectID: f.project.String(), EnvironmentID: f.env.String(), AgentID: f.agent.String()}
			principal, err := access.Principal(t.Context(), f.pool, "api_key", f.key, f.env)
			if err != nil {
				t.Fatal(err)
			}
			// Each action works independently within its assigned scope.
			if !principal.HasPermission(definition.ID, scope) {
				t.Fatal("assignment did not preserve exact operation semantics")
			}
			if err := org.RemoveRoleGrant(t.Context(), f.pool, f.owner, rows[0].ID, strconv.FormatInt(rows[0].Revision, 10)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
