package org_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"testing"
	"uuid"
)

func TestBootstrapExplicitGrantsAndOwnerRecovery(t *testing.T) {
	d := dbtest.Open(t)
	ctx := t.Context()
	if err := schema.Up(ctx, d.DSN); err != nil {
		t.Fatal(err)
	}
	user := insertUser(t, d.Pool, "Founder", "founder@example.test")
	organization, err := identity.CreateOrganization(ctx, d.Pool, org.OrganizationInput{Actor: audit.Internal(audit.ControlPlane), Slug: "bootstrap", OwnerUserID: user, AuditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}}, dbtest.BrowserPrincipal(t, d.Pool, uuid.Nil(), user), auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	orgID := pgvalue.MustUUIDValue(organization.ID)
	principal, err := access.OrganizationPrincipal(ctx, d.Pool, orgID, user)
	if err != nil {
		t.Fatal(err)
	}
	m := org.ManagingMember{Human: dbtest.BrowserPrincipal(t, d.Pool, orgID, user), Actor: audit.VerifiedLogin(user, "fixture"), OrgID: orgID, UserID: user, MemberID: principal.MemberID, AuditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}}
	roles, err := org.ListPermissionRoles(ctx, d.Pool, m)
	if err != nil || len(roles) != 17 {
		t.Fatalf("roles: %d %v", len(roles), err)
	}
	var groupCount, grantCount int
	if err = d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM permission_groups),(SELECT count(*) FROM role_grants)`).Scan(&groupCount, &grantCount); err != nil || groupCount != 0 || grantCount != 3 {
		t.Fatalf("initial policy: %d %d %v", groupCount, grantCount, err)
	}
	if !principal.IsOwner || len(principal.Grants) != 27 {
		t.Fatalf("bootstrap principal: %+v", principal)
	}
	if !principal.HasPermission(auth.PermissionAuditEventsRead, auth.Scope{OrgID: orgID}) {
		t.Fatal("organization creator lacks audit access")
	}
	var grantID uuid.UUID
	// Remove Auditor and Organization Structure Administrator before Access Administrator.
	for _, capability := range []string{"audit_events.read", "projects.create", "assignments.create"} {
		if err = d.Pool.QueryRow(ctx, `SELECT g.id FROM role_grants g JOIN roles r ON r.id=g.role_id CROSS JOIN LATERAL unnest(r.permissions) p(permission) WHERE g.org_id=$1 AND p.permission=$2`, orgID, capability).Scan(&grantID); err != nil {
			t.Fatal(err)
		}
		if err = org.RemoveRoleGrant(ctx, d.Pool, m, grantID, "1"); err != nil {
			t.Fatal(err)
		}
	}
	principal, err = access.OrganizationPrincipal(ctx, d.Pool, orgID, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(principal.Grants) != 0 || !principal.IsOwner {
		t.Fatalf("ownership silently restored grants: %+v", principal)
	}
	if _, err = org.ListPermissionRoles(ctx, d.Pool, m); !errors.Is(err, org.ErrMemberManagementRequired) {
		t.Fatalf("owner bypass: %v", err)
	}
	if _, err = org.OwnershipMembers(ctx, d.Pool, m); err != nil {
		t.Fatal(err)
	}
	recovered, err := org.RecoverOwnAccess(ctx, d.Pool, m)
	if err != nil || !recovered.Created {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
	again, err := org.RecoverOwnAccess(ctx, d.Pool, m)
	if err != nil || again.Created || again.ID != recovered.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	principal, err = access.OrganizationPrincipal(ctx, d.Pool, orgID, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(principal.Grants) != 19 || !principal.HasPermission(auth.PermissionAssignmentsCreate, auth.Scope{OrgID: orgID}) {
		t.Fatalf("recovery exceeded fixed grant: %+v", principal.Grants)
	}
	var recoveries int
	if err = d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='access.recovered'`).Scan(&recoveries); err != nil || recoveries != 1 {
		t.Fatalf("audit: %d %v", recoveries, err)
	}
}

func TestRemovedMembershipCannotReceiveAssignmentsAfterRejoin(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	group, err := org.SavePermissionGroup(ctx, f.pool, f.owner, uuid.Nil(), "Membership lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err = org.SetPermissionGroupMember(ctx, f.pool, f.owner, group.ID, f.member.MemberID, true); err != nil {
		t.Fatal(err)
	}
	role, err := org.SavePermissionRole(ctx, f.pool, f.owner, uuid.Nil(), org.RoleInput{Name: "Lifecycle organization manager", Permissions: []string{"projects.create"}})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := org.AssignRole(ctx, f.pool, f.owner, org.GrantInput{Subject: org.GrantSubject{Type: "member", MemberID: &f.member.MemberID}, RoleID: role.ID, AgentsMode: "none", ExpectedRoleRevision: role.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err = identity.RemoveMember(ctx, f.pool, f.owner, f.member.MemberID); err != nil {
		t.Fatal(err)
	}
	var grantName, memberID, groupName, userID string
	if err := f.pool.QueryRow(ctx, `SELECT details->'before'->>'role_name',details->'before'->>'member_id' FROM audit_events WHERE action='role_grant.deleted' AND target_id=$1`, grants[0].ID).Scan(&grantName, &memberID); err != nil || grantName != role.Name || memberID != f.member.MemberID.String() {
		t.Fatalf("grant removal snapshot: %s %s %v", grantName, memberID, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT details->'before'->>'group_name',details->'before'->>'member_id',details->'before'->>'user_id' FROM audit_events WHERE action='permission_group.member_removed' AND target_id=$1`, group.ID).Scan(&groupName, &memberID, &userID); err != nil || groupName != group.Name || memberID != f.member.MemberID.String() || userID != f.member.UserID.String() {
		t.Fatalf("membership removal snapshot: %s %s %s %v", groupName, memberID, userID, err)
	}
	joined, err := f.queries.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{ID: pgvalue.NewUUIDv7(), OrgID: pgvalue.UUID(f.owner.OrgID), UserID: pgvalue.UUID(f.member.UserID)})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(joined.ID) == f.member.MemberID {
		t.Fatal("membership identity reused")
	}
	if err = org.SetPermissionGroupMember(ctx, f.pool, f.owner, group.ID, f.member.MemberID, true); !errors.Is(err, org.ErrMemberNotFound) {
		t.Fatalf("stale membership assigned: %v", err)
	}
	next, err := org.GetPermissionGroup(ctx, f.pool, f.owner, group.ID)
	if err != nil || len(next.MemberIDs) != 0 || next.Revision <= group.Revision {
		t.Fatalf("rejoin retained group access: %+v %v", next, err)
	}
	p, err := access.OrganizationPrincipal(ctx, f.pool, f.owner.OrgID, f.member.UserID)
	if err != nil || len(p.Grants) != 0 {
		t.Fatalf("rejoined access: %+v %v", p, err)
	}
}

func TestOwnershipDemotionAndMemberRemovalSerialize(t *testing.T) {
	f := newMemberFixture(t)
	if _, err := org.UpdateMemberOwnership(t.Context(), f.pool, f.owner, f.member.MemberID, true, false); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	demoted := make(chan error, 1)
	removed := make(chan error, 1)
	go func() {
		<-start
		_, err := org.UpdateMemberOwnership(t.Context(), f.pool, f.owner, f.member.MemberID, false, true)
		demoted <- err
	}()
	go func() { <-start; removed <- identity.RemoveMember(t.Context(), f.pool, f.owner, f.member.MemberID) }()
	close(start)
	if err := <-demoted; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err != nil && !errors.Is(err, org.ErrOwnerRoleRequired) {
		t.Fatal(err)
	}
	var invalid, owners int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE is_owner AND removed_at IS NOT NULL),count(*) FILTER(WHERE is_owner AND removed_at IS NULL) FROM org_members WHERE org_id=$1`, f.owner.OrgID).Scan(&invalid, &owners); err != nil || invalid != 0 || owners != 1 {
		t.Fatalf("ownership after race: invalid=%d owners=%d %v", invalid, owners, err)
	}
}
