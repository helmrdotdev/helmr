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
)

func TestAllMembersUsesNormalRoleTargets(t *testing.T) {
	f := newMachineGrantFixture(t)
	ctx := t.Context()
	role := f.role(t, "sessions.read", "secrets.read")
	input := f.input(role)
	input.Subject = org.GrantSubject{Type: "all_members"}
	grants, err := org.AssignRole(ctx, f.pool, f.owner, input)
	if err != nil {
		t.Fatal(err)
	}
	check := func(user, env, agent uuid.UUID, permission auth.Permission, allowed bool) {
		t.Helper()
		err := access.Check(ctx, f.pool, "user", user, env, agent, permission)
		if allowed && err != nil || !allowed && !errors.Is(err, access.ErrDenied) {
			t.Fatalf("%s allowed=%t: %v", permission, allowed, err)
		}
	}
	check(f.member.UserID, f.env, f.agent, auth.PermissionSessionsRead, true)
	check(f.member.UserID, f.env, f.otherAgent, auth.PermissionSessionsRead, false)
	check(f.member.UserID, f.env, f.otherAgent, auth.PermissionSecretsRead, true)
	check(f.member.UserID, f.otherEnv, f.agent, auth.PermissionSecretsRead, false)
	f.check(t, f.key, f.agent, f.env, auth.PermissionSessionsRead, false)
	input.AgentsMode = "all"
	input.AgentIDs = nil
	input.ExpectedRevision = strconv.FormatInt(grants[0].Revision, 10)
	updated, err := org.UpdateRoleGrant(ctx, f.pool, f.owner, grants[0].ID, input)
	if err != nil {
		t.Fatal(err)
	}
	future := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'future')`, future, f.env)
	user := insertUser(t, f.pool, "Future", "future@example.test")
	member := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, member, f.owner.OrgID, user)
	check(user, f.env, future, auth.PermissionSessionsRead, true)
	check(f.member.UserID, f.env, f.otherAgent, auth.PermissionSessionsRead, true)
	direct := f.input(role)
	direct.Subject = org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}
	if _, err := org.AssignRole(ctx, f.pool, f.owner, direct); err != nil {
		t.Fatal(err)
	}
	if err := org.RemoveRoleGrant(ctx, f.pool, f.owner, updated.ID, strconv.FormatInt(updated.Revision, 10)); err != nil {
		t.Fatal(err)
	}
	check(user, f.env, future, auth.PermissionSessionsRead, false)
	check(f.member.UserID, f.env, f.agent, auth.PermissionSessionsRead, true)
	check(f.member.UserID, f.env, f.otherAgent, auth.PermissionSessionsRead, false)
	organization := f.role(t, "members.read")
	if _, err := org.AssignRole(ctx, f.pool, f.owner, org.GrantInput{RoleID: organization.ID, ExpectedRoleRevision: organization.Revision, Subject: org.GrantSubject{Type: "all_members"}, AgentsMode: "none"}); err != nil {
		t.Fatal("organization Role", err)
	}
}
