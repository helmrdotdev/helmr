package identity

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"testing"
	"uuid"
)

func TestMemberAccessLifecyclePreservesOtherOrganization(t *testing.T) {
	f := newIdentityFixture(t)
	a, b := f.organization(t, "first"), f.organization(t, "second")
	owner, user := f.user(t, "Owner", "owner@example.test"), f.user(t, "Member", "member@example.test")
	ownerID := f.member(t, a, owner, "owner")
	member := f.member(t, a, user, "member")
	f.member(t, b, user, "member")
	dbtest.GrantMember(t, f.pool, a, owner, nil, "members.remove")
	managerProof := dbtest.BrowserPrincipal(t, f.pool, a, owner)
	manager := org.ManagingMember{Human: managerProof, Actor: audit.FromPrincipal(managerProof), OrgID: a, UserID: owner, MemberID: ownerID, AuditPolicy: f.cfg.auditPolicy}
	token := f.session(t, user, a)
	check := func(raw string, organization uuid.UUID, allowed bool) {
		t.Helper()
		_, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, organization)
		if (err == nil) != allowed {
			t.Fatalf("org %s allowed=%v: %v", organization, allowed, err)
		}
	}
	check(token, a, true)
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, member, nil); err != nil {
		t.Fatal(err)
	}
	check(token, a, false)
	check(token, b, true)
	check(token, uuid.Nil(), true)
	fresh := f.session(t, user, a)
	check(fresh, a, true)
	suspended := true
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, member, &suspended); err != nil {
		t.Fatal(err)
	}
	check(fresh, a, false)
	if _, err := org.UpdateMemberOwnership(t.Context(), f.pool, manager, member, true, false); err == nil {
		t.Fatal("suspended member promoted")
	}
	candidates, err := org.OwnershipMembers(t.Context(), f.pool, manager)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.MembershipID == member {
			t.Fatal("suspended succession candidate")
		}
	}
	check(fresh, b, true)
	during := f.session(t, user, b)
	check(during, a, false)
	suspended = false
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, member, &suspended); err != nil {
		t.Fatal(err)
	}
	check(during, a, false)
	check(f.session(t, user, a), a, true)
	suspended = true
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, ownerID, &suspended); err == nil {
		t.Fatal("owner suspended")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action IN ('member.sessions_revoked','member.suspended','member.resumed')`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	// Both mutations serialize on the org root; exactly one may win.
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; v := true; results <- ChangeMemberAccess(t.Context(), f.pool, manager, member, &v) }()
	go func() {
		<-start
		_, err := org.UpdateMemberOwnership(t.Context(), f.pool, manager, member, true, false)
		results <- err
	}()
	close(start)
	wins := 0
	for range 2 {
		if <-results == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("promotion/suspension successes", wins)
	}
	var impossible bool
	if err := f.pool.QueryRow(t.Context(), `SELECT is_owner AND suspended_at IS NOT NULL FROM org_members WHERE id=$1`, member).Scan(&impossible); err != nil || impossible {
		t.Fatal("suspended owner", impossible, err)
	}
}

func TestFirstOrganizationSetupSerializesConcurrentAccounts(t *testing.T) {
	f := newIdentityFixture(t)
	operator := dbtest.OperatorPrincipal(t, f.pool)
	accounts := make([]auth.Principal, 2)
	for i := range accounts {
		user := f.user(t, fmt.Sprintf("Owner%d", i), fmt.Sprintf("owner%d@example.test", i))
		raw := f.session(t, user, uuid.Nil())
		var err error
		accounts[i], err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, p := range accounts {
		go func() {
			<-start
			_, err := CreateOrganization(t.Context(), f.pool, org.OrganizationInput{AuditPolicy: f.cfg.auditPolicy, Actor: audit.FromPrincipal(p), Slug: fmt.Sprintf("setup-%d", i), Name: "Setup", OwnerUserID: p.UserID, InitialSetup: true}, p, operator)
			results <- err
		}()
	}
	close(start)
	var successes int
	for range accounts {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, org.ErrOrganizationExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("successful setup count", successes)
	}
}
