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

func TestAssignmentTargetsRemainIndependent(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	m := f.owner
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO regions(id,display_name) VALUES('typed','Typed')`)
	project, envs, err := org.CreateProject(ctx, f.pool, m.OrgID, org.ProjectInput{ProjectDetails: org.ProjectDetails{Slug: "typed", Name: "Typed"}, ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	prod, stage := pgvalue.MustUUIDValue(envs[0].ID), pgvalue.MustUUIDValue(envs[1].ID)
	faq, invoice, stageInvoice := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'faq'),($3,$2,'invoice'),($4,$5,'invoice')`, faq, prod, invoice, stageInvoice, stage)
	role, err := org.SavePermissionRole(ctx, f.pool, m, uuid.Nil(), org.RoleInput{Name: "Scoped tools", Permissions: []string{"agents.use"}})
	if err != nil {
		t.Fatal(err)
	}
	input := org.GrantInput{ProjectID: &projectID, EnvironmentID: &prod, Subject: org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}, RoleID: role.ID, ExpectedRoleRevision: role.Revision, AgentsMode: "selected", AgentIDs: []uuid.UUID{faq}}
	rows, err := org.AssignRole(ctx, f.pool, m, input)
	if err != nil {
		t.Fatal(err)
	}
	grant := rows[0]
	check := func(env, agent uuid.UUID, p auth.Permission, want bool) {
		t.Helper()
		err := access.Check(ctx, f.pool, "user", f.member.UserID, env, agent, p)
		if want && err != nil || !want && !errors.Is(err, access.ErrDenied) {
			t.Fatalf("%s %s %s allowed=%v: %v", env, agent, p, want, err)
		}
	}
	// Adding an environment-only action preserves the existing Agent target.
	role, err = org.SavePermissionRole(ctx, f.pool, m, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"agents.use", "secrets.manage"}})
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedRoleRevision = role.Revision
	refreshed, err := org.QueryRoleGrants(ctx, f.pool, m, org.GrantFilter{ID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	grant = refreshed[0]
	check(prod, faq, auth.CapabilityAgentsUse, true)
	check(prod, invoice, auth.CapabilityAgentsUse, false)
	check(prod, uuid.Nil(), auth.CapabilitySecretsManage, true)
	check(stage, stageInvoice, auth.CapabilityAgentsUse, false)
	if len(grant.Targets) != 2 || grant.Targets[0].ResourceType != "agents" || grant.Targets[0].Mode != "selected" || grant.Targets[1].Mode != "environment" {
		t.Fatalf("incorrect grouped targets: %+v", grant.Targets)
	}
	again, err := org.AssignRole(ctx, f.pool, m, input)
	if err != nil || again[0].Created || again[0].ID != grant.ID {
		t.Fatalf("equivalent POST: %+v %v", again, err)
	}
	input.AgentIDs = []uuid.UUID{invoice}
	if _, err := org.AssignRole(ctx, f.pool, m, input); !errors.Is(err, org.ErrGrantExists) {
		t.Fatalf("different POST overwrote targets: %v", err)
	}
	input.AgentIDs = []uuid.UUID{stageInvoice}
	input.ExpectedRevision = strconv.FormatInt(grant.Revision, 10)
	if _, err := org.UpdateRoleGrant(ctx, f.pool, m, grant.ID, input); !errors.Is(err, org.ErrAccessNotFound) {
		t.Fatalf("cross-environment target accepted: %v", err)
	}
	input.AgentIDs = []uuid.UUID{faq}
	input.EnvironmentID = &stage
	input.AgentIDs = []uuid.UUID{stageInvoice}
	input.ExpectedRevision = ""
	if _, err := org.AssignRole(ctx, f.pool, m, input); err != nil {
		t.Fatal(err)
	}
	check(stage, stageInvoice, auth.CapabilityAgentsUse, true)
	// Session management and its included viewing retain the same Agent targets.
	role, err = org.SavePermissionRole(ctx, f.pool, m, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"agents.manage", "sessions.manage", "secrets.manage"}})
	if err != nil {
		t.Fatal(err)
	}
	check(prod, invoice, auth.CapabilityAgentsUse, false)
	check(prod, faq, auth.CapabilitySessionsView, true)
	check(prod, invoice, auth.CapabilitySessionsView, false)
	check(prod, faq, auth.CapabilityAgentsUse, false)
	check(prod, faq, auth.CapabilityAgentsManage, true)
	check(prod, invoice, auth.CapabilityAgentsManage, false)
	principal, err := access.OrganizationPrincipal(ctx, f.pool, m.OrgID, m.UserID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := access.MemberPermissions(ctx, f.pool, principal, f.member.MemberID, access.MemberPermissionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range effective.Scopes {
		if scope.Environment == envs[0].Slug && scope.Agent == "" {
			for _, permission := range scope.Permissions {
				if permission == "agents.manage" {
					t.Fatal("inspection widened Agent management")
				}
			}
		}
	}
	// Removing Agent actions clears selections; adding them cannot resurrect targets.
	role, err = org.SavePermissionRole(ctx, f.pool, m, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"secrets.manage"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := org.QueryRoleGrants(ctx, f.pool, m, org.GrantFilter{ID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current[0].AgentsMode != "none" || len(current[0].AgentIDs) != 0 || current[0].Revision != grant.Revision+1 {
		t.Fatalf("latent targets: %+v", current[0])
	}
	if _, err := org.SavePermissionRole(ctx, f.pool, m, role.ID, org.RoleInput{Name: role.Name, ExpectedRevision: strconv.FormatInt(role.Revision, 10), Permissions: []string{"agents.use", "secrets.manage"}}); !errors.Is(err, org.ErrRoleTarget) {
		t.Fatalf("Agent targets resurrected: %v", err)
	}
	var targetAudit bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='role.updated' AND target_id=$1 AND details->'before_assignments' @> $2::jsonb AND details->'after_assignments' @> $3::jsonb)`, role.ID, `[{"grant_id":"`+grant.ID.String()+`","agent_keys":["faq"]}]`, `[{"grant_id":"`+grant.ID.String()+`","agents_mode":"none"}]`).Scan(&targetAudit); err != nil || !targetAudit {
		t.Fatalf("role target audit missing: %v", err)
	}
	check(prod, faq, auth.CapabilityAgentsUse, false)
	check(prod, uuid.Nil(), auth.CapabilitySecretsManage, true)
	if err := org.RemoveRoleGrant(ctx, f.pool, m, grant.ID, strconv.FormatInt(grant.Revision, 10)); !errors.Is(err, org.ErrStaleRevision) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := org.RemoveRoleGrant(ctx, f.pool, m, grant.ID, strconv.FormatInt(current[0].Revision, 10)); err != nil {
		t.Fatal(err)
	}
	if err := org.RemoveRoleGrant(ctx, f.pool, m, grant.ID, "1"); err != nil {
		t.Fatalf("absent delete: %v", err)
	}
	// A targeted Agent cannot be physically deleted without an explicit assignment change.
	selectedRole, err := org.SavePermissionRole(ctx, f.pool, m, uuid.Nil(), org.RoleInput{Name: "Deletion", Permissions: []string{"agents.use", "secrets.manage"}})
	if err != nil {
		t.Fatal(err)
	}
	input.RoleID = selectedRole.ID
	input.ExpectedRoleRevision = selectedRole.Revision
	input.EnvironmentID = &prod
	input.AgentIDs = []uuid.UUID{faq}
	input.ExpectedRevision = ""
	rows, err = org.AssignRole(ctx, f.pool, m, input)
	if err != nil {
		t.Fatal(err)
	}
	grant = rows[0]
	_, err = f.pool.Exec(ctx, `DELETE FROM agents WHERE environment_id=$1 AND id=$2`, prod, faq)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23001" {
		t.Fatalf("targeted Agent deletion: %v", err)
	}
	current, err = org.QueryRoleGrants(ctx, f.pool, m, org.GrantFilter{ID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(current[0].AgentIDs) != 1 || current[0].AgentIDs[0] != faq || current[0].Revision != grant.Revision {
		t.Fatalf("rejected delete changed targets: %+v", current[0])
	}
	check(prod, faq, auth.CapabilityAgentsUse, true)
	// No product mutation currently creates an empty selection. Construct one to
	// retain the fail-closed read/repair contract without a deletion side effect.
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grant_agents WHERE grant_id=$1`, grant.ID)
	current, err = org.QueryRoleGrants(ctx, f.pool, m, org.GrantFilter{ID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current[0].AgentsMode != "selected" || len(current[0].AgentIDs) != 0 || current[0].Revision != grant.Revision {
		t.Fatalf("empty target was broadened: %+v", current[0])
	}
	check(prod, faq, auth.CapabilityAgentsUse, false)
	check(prod, invoice, auth.CapabilityAgentsUse, false)
	check(prod, uuid.Nil(), auth.CapabilitySecretsManage, true)
	input.AgentIDs = []uuid.UUID{invoice}
	input.ExpectedRevision = strconv.FormatInt(current[0].Revision, 10)
	repaired, err := org.UpdateRoleGrant(ctx, f.pool, m, grant.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Revision != grant.Revision+1 {
		t.Fatalf("replacement revision = %d, want %d", repaired.Revision, grant.Revision+1)
	}
	input.ExpectedRevision = strconv.FormatInt(grant.Revision, 10)
	if _, err := org.UpdateRoleGrant(ctx, f.pool, m, grant.ID, input); !errors.Is(err, org.ErrStaleRevision) {
		t.Fatalf("stale repair: %v", err)
	}
	check(prod, invoice, auth.CapabilityAgentsUse, true)
	// Concurrent complete replacements permit exactly one writer at an observed revision.
	input.ExpectedRevision = strconv.FormatInt(repaired.Revision, 10)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := org.UpdateRoleGrant(ctx, f.pool, m, repaired.ID, input); results <- err }()
	}
	close(start)
	success, stale := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, org.ErrStaleRevision) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("replacement race successes=%d stale=%d", success, stale)
	}
	// Sharing another Agent races last-Agent unshare against the same observed header.
	current, err = org.QueryRoleGrants(ctx, f.pool, m, org.GrantFilter{ID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = strconv.FormatInt(current[0].Revision, 10)
	another := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'later')`, another, prod)
	input.AgentIDs = []uuid.UUID{invoice, another}
	raceInput := input
	raceStart := make(chan struct{})
	patchResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-raceStart
		_, err := org.UpdateRoleGrant(ctx, f.pool, f.admin, grant.ID, raceInput)
		patchResult <- err
	}()
	go func() {
		<-raceStart
		deleteResult <- org.RemoveRoleGrant(ctx, f.pool, m, grant.ID, raceInput.ExpectedRevision)
	}()
	close(raceStart)
	patchErr, deleteErr := <-patchResult, <-deleteResult
	if !(patchErr == nil && errors.Is(deleteErr, org.ErrStaleRevision) || deleteErr == nil && errors.Is(patchErr, org.ErrAccessNotFound)) {
		t.Fatalf("unshare/add race: patch=%v delete=%v", patchErr, deleteErr)
	}
	// The same Role supports different selections for different people and explicit all includes future Agents.
	input.Subject = org.GrantSubject{Type: "member", MemberID: &f.admin.MemberID}
	input.AgentsMode = "all"
	input.AgentIDs = nil
	input.ExpectedRevision = ""
	all, err := org.AssignRole(ctx, f.pool, m, input)
	if err != nil {
		t.Fatal(err)
	}
	future := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'future')`, future, prod)
	if err := access.Check(ctx, f.pool, "user", f.admin.UserID, prod, future, auth.CapabilityAgentsUse); err != nil {
		t.Fatalf("all did not include future Agent: %v", err)
	}
	check(prod, future, auth.CapabilityAgentsUse, false)
	input.ExpectedRevision = strconv.FormatInt(all[0].Revision, 10)
	input.AgentsMode = "selected"
	input.AgentIDs = []uuid.UUID{invoice}
	if _, err := org.UpdateRoleGrant(ctx, f.pool, m, all[0].ID, input); err != nil {
		t.Fatal(err)
	}
	if err := access.Check(ctx, f.pool, "user", f.admin.UserID, prod, future, auth.CapabilityAgentsUse); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("selected included future Agent: %v", err)
	}

}

func TestCatalogAgentTargetsAndInspectionVisibility(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	m := f.owner
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO regions(id,display_name) VALUES('catalog','Catalog')`)
	project, envs, err := org.CreateProject(ctx, f.pool, m.OrgID, org.ProjectInput{ProjectDetails: org.ProjectDetails{Slug: "catalog", Name: "Catalog"}, ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}})
	if err != nil {
		t.Fatal(err)
	}
	projectID, env := pgvalue.MustUUIDValue(project.ID), pgvalue.MustUUIDValue(envs[0].ID)
	selected, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'selected'),($3,$2,'other')`, selected, env, other)
	for _, definition := range auth.CapabilityCatalog() {
		if definition.ResourceType != "agents" {
			continue
		}
		t.Run(string(definition.ID), func(t *testing.T) {
			role, err := org.SavePermissionRole(ctx, f.pool, m, uuid.Nil(), org.RoleInput{Name: string(definition.ID), Permissions: []string{string(definition.ID)}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = org.AssignRole(ctx, f.pool, m, org.GrantInput{ProjectID: &projectID, EnvironmentID: &env, Subject: org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}, RoleID: role.ID, ExpectedRoleRevision: role.Revision, AgentsMode: "selected", AgentIDs: []uuid.UUID{selected}})
			if err != nil {
				t.Fatal(err)
			}
			if err := access.Check(ctx, f.pool, "user", f.member.UserID, env, selected, definition.ID); err != nil {
				t.Fatalf("selected target: %v", err)
			}
			if err := access.Check(ctx, f.pool, "user", f.member.UserID, env, other, definition.ID); !errors.Is(err, access.ErrDenied) {
				t.Fatalf("catalog Agent capability escaped selection: %v", err)
			}
		})
	}
	principal, err := access.OrganizationPrincipal(ctx, f.pool, m.OrgID, f.member.UserID)
	if err != nil {
		t.Fatal(err)
	}
	inspect := func(agent string, wantVisible bool) {
		t.Helper()
		_, err := access.MemberPermissions(ctx, f.pool, principal, f.member.MemberID, access.MemberPermissionFilter{Project: project.Slug, Environment: envs[0].Slug, Agent: agent})
		if wantVisible && err != nil || !wantVisible && !errors.Is(err, access.ErrNotFound) {
			t.Fatalf("inspect %s visible=%v: %v", agent, wantVisible, err)
		}
	}
	inspect("selected", true)
	inspect("other", false)
	dbtest.MustExec(t, ctx, f.pool, `DELETE FROM role_grants WHERE org_id=$1 AND member_id=$2`, m.OrgID, f.member.MemberID)
	dbtest.GrantMember(t, f.pool, m.OrgID, f.member.UserID, &env, "secrets.manage")
	inspect("selected", false)
	inspect("other", false)
	inspect("missing", false)
	// An environment-wide implication of Agent use legitimately reveals targets.
	dbtest.GrantMember(t, f.pool, m.OrgID, f.member.UserID, &env, "sessions.manage")
	inspect("other", true)
	inspect("missing", false)
}
