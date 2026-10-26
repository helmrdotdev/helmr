package org_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestGrantSchemaRejectsInvalidSubjectAndTenant(t *testing.T) {
	f := newMemberFixture(t)
	role, err := org.SavePermissionRole(t.Context(), f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: "Constraint manager", Permissions: []string{"assignments.create"}})
	if err != nil {
		t.Fatal(err)
	}
	group, err := org.SavePermissionGroup(t.Context(), f.pool, f.owner, uuid.Nil(), "Constraint group")
	if err != nil {
		t.Fatal(err)
	}
	foreignOrg, foreignGroup := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Foreign','constraint-foreign')`, foreignOrg)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO permission_groups(id,org_id,name) VALUES($1,$2,'Foreign group')`, foreignGroup, foreignOrg)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO regions(id,display_name) VALUES('constraints','Constraints')`)
	input := org.ProjectInput{Slug: "constraints", Name: "Constraints", ExecutionLimits: org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}}
	_, environments, err := org.CreateProject(t.Context(), f.pool, f.owner, input)
	if err != nil {
		t.Fatal(err)
	}
	foreignMember := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, foreignMember, foreignOrg, f.owner.UserID)
	dbtest.GrantMember(t, f.pool, foreignOrg, f.owner.UserID, nil, "projects.create")
	_, foreignEnvironments, err := org.CreateProject(t.Context(), f.pool, org.ManagingMember{OrgID: foreignOrg, UserID: f.owner.UserID, MemberID: foreignMember, Actor: f.owner.Actor, AuditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}}, input)
	if err != nil {
		t.Fatal(err)
	}
	environment, otherEnvironment, foreignEnvironment := pgvalue.MustUUIDValue(environments[0].ID), pgvalue.MustUUIDValue(environments[1].ID), pgvalue.MustUUIDValue(foreignEnvironments[0].ID)
	otherAgent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'other-agent')`, otherEnvironment, otherAgent)
	for _, tc := range []struct {
		name                              string
		member, group, agent, environment *uuid.UUID
		all                               bool
		code                              string
	}{
		{name: "no subject", code: "23514"},
		{name: "multiple subjects", member: &f.member.MemberID, group: &group.ID, code: "23514"},
		{name: "audience organization scope", all: true, code: "23514"},
		{name: "agent without environment", member: &f.member.MemberID, agent: new(uuid.NewV7()), code: "23502"},
		{name: "foreign group", group: &foreignGroup, code: "23503"},
		{name: "foreign environment with null agent", member: &f.member.MemberID, environment: &foreignEnvironment, code: "23503"},
		{name: "agent from another environment", member: &f.member.MemberID, environment: &environment, agent: &otherAgent, code: "23503"},
	} {
		t.Run(tc.name, func(t *testing.T) {

			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			id := uuid.NewV7()
			_, err = tx.Exec(t.Context(), `INSERT INTO role_grants(id,org_id,role_id,member_id,group_id,all_members,environment_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, f.owner.OrgID, role.ID, tc.member, tc.group, tc.all, tc.environment)
			if err == nil && tc.agent != nil {
				_, err = tx.Exec(t.Context(), `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) VALUES($1,$2,$3)`, id, tc.environment, tc.agent)
			}

			var constraint *pgconn.PgError
			if !errors.As(err, &constraint) || constraint.Code != tc.code {
				t.Fatalf("want constraint %s, got %v", tc.code, err)
			}
		})
	}
}
