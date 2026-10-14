package org_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMemberManagementPostgresEnforcesOwnerAuthority(t *testing.T) {
	fixture := newMemberFixture(t)
	ctx := t.Context()
	q := fixture.queries
	owner, admin, developer := fixture.owner, fixture.admin, fixture.developer

	t.Run("principals without member management are rejected", func(t *testing.T) {
		for _, principal := range []org.ManagingMember{developer, fixture.viewer} {
			if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, principal, org.InvitationInput{Emails: []string{"blocked@example.test"}, Role: "viewer"}); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s invitation error = %v", principal.Role, err)
			}
			if err := org.RevokeInvitation(ctx, fixture.pool, principal, uuid.NewV7()); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s revoke error = %v", principal.Role, err)
			}
			if _, err := org.UpdateMemberRole(ctx, fixture.pool, principal, fixture.viewer.UserID, "developer", "viewer"); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s role change error = %v", principal.Role, err)
			}
			if err := identity.RemoveMember(ctx, fixture.pool, principal, fixture.viewer.UserID); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s removal error = %v", principal.Role, err)
			}
			if _, err := org.ListMembers(ctx, q, principal); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s member list error = %v", principal.Role, err)
			}
			if _, _, err := org.ListInvitations(ctx, q, principal, 10, nil); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s invitation list error = %v", principal.Role, err)
			}
		}
		var blocked int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email = 'blocked@example.test'`).Scan(&blocked); err != nil || blocked != 0 {
			t.Fatalf("blocked invitations = %d, err = %v", blocked, err)
		}
	})

	t.Run("invitations", func(t *testing.T) {
		if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"next-owner@example.test"}, Role: "owner"}); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation error = %v", err)
		}
		invitation, rawToken, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"Teammate@Example.test"}, Role: "developer"})
		if err != nil || rawToken == "" || invitation.InviteeEmail != "teammate@example.test" {
			t.Fatalf("invitation = %+v, token set %t, err = %v", invitation, rawToken != "", err)
		}
		if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"teammate@example.test"}, Role: "viewer"}); !errors.Is(err, org.ErrInvitationPending) {
			t.Fatalf("duplicate invitation error = %v", err)
		}
		if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"developer@example.test"}, Role: "viewer"}); !errors.Is(err, org.ErrInvitationActiveMember) {
			t.Fatalf("active member invitation error = %v", err)
		}
		days := 31
		var input org.InputError
		if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"later@example.test"}, Role: "viewer", ExpiresInDays: &days}); !errors.As(err, &input) {
			t.Fatalf("expiry error = %v, want input error", err)
		}

		ownerInvitation, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, owner, org.InvitationInput{Emails: []string{"next-owner@example.test"}, Role: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"next-owner@example.test"}, Role: "viewer"}); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin invitation over pending owner invitation error = %v", err)
		}
		ownerInvitationID := pgvalue.MustUUIDValue(ownerInvitation.ID)
		if err := org.RevokeInvitation(ctx, fixture.pool, admin, ownerInvitationID); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation revoke error = %v", err)
		}
		if err := org.RevokeInvitation(ctx, fixture.pool, owner, ownerInvitationID); err != nil {
			t.Fatal(err)
		}
		if err := org.RevokeInvitation(ctx, fixture.pool, owner, ownerInvitationID); !errors.Is(err, org.ErrInvitationNotFound) {
			t.Fatalf("second revoke error = %v", err)
		}
	})

	t.Run("role changes", func(t *testing.T) {
		updated, err := org.UpdateMemberRole(ctx, fixture.pool, admin, developer.UserID, "viewer", "developer")
		if err != nil || updated.Member.Role != db.OrgMemberRoleViewer || updated.PrimaryEmail.String != "developer@example.test" {
			t.Fatalf("updated = %+v, err = %v", updated, err)
		}
		if _, err := org.UpdateMemberRole(ctx, fixture.pool, admin, developer.UserID, "developer", "developer"); !errors.Is(err, org.ErrMemberRoleChanged) {
			t.Fatalf("stale expected role error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, fixture.pool, admin, developer.UserID, "owner", "viewer"); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin promotion to owner error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, fixture.pool, admin, admin.UserID, "developer", "admin"); !errors.Is(err, org.ErrSelfMemberManagement) {
			t.Fatalf("self demotion error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, fixture.pool, owner, owner.UserID, "admin", "owner"); !errors.Is(err, org.ErrLastActiveOwner) {
			t.Fatalf("last owner demotion error = %v", err)
		}
	})

	t.Run("removal", func(t *testing.T) {
		if err := identity.RemoveMember(ctx, fixture.pool, admin, admin.UserID); !errors.Is(err, org.ErrSelfMemberRemoval) {
			t.Fatalf("self removal error = %v", err)
		}
		if err := identity.RemoveMember(ctx, fixture.pool, admin, owner.UserID); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner removal error = %v", err)
		}
		if err := identity.RemoveMember(ctx, fixture.pool, admin, developer.UserID); err != nil {
			t.Fatal(err)
		}
		if err := identity.RemoveMember(ctx, fixture.pool, admin, developer.UserID); !errors.Is(err, org.ErrMemberNotFound) {
			t.Fatalf("second removal error = %v", err)
		}
		members, err := org.ListMembers(ctx, q, owner)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			if pgvalue.MustUUIDValue(member.UserID) == developer.UserID && !member.DisabledAt.Valid {
				t.Fatal("removed member is still active")
			}
		}
	})
}

func TestCreateOrganizationPostgresInitialSetupAdmitsOneOrganization(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	userID := insertUser(t, database.Pool, "Owner", "owner@example.test")
	created, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "Acme", OwnerUserID: userID, InitialSetup: true})
	if err != nil || created.Slug != "acme" || created.Name != "acme" {
		t.Fatalf("organization = %+v, err = %v", created, err)
	}
	var admin bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT admin FROM users WHERE id = $1`, userID).Scan(&admin); err != nil || !admin {
		t.Fatalf("initial owner admin = %t, err = %v", admin, err)
	}
	member, err := queries.GetOrgMemberForManagement(t.Context(), db.GetOrgMemberForManagementParams{OrgID: created.ID, UserID: pgvalue.UUID(userID)})
	if err != nil || member.DisabledAt.Valid || member.Role != db.OrgMemberRoleOwner {
		t.Fatalf("owner member = %+v, err = %v", member, err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "second", OwnerUserID: userID, InitialSetup: true}); !errors.Is(err, org.ErrOrganizationExists) {
		t.Fatalf("second setup error = %v", err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "acme", OwnerUserID: userID}); !errors.Is(err, org.ErrOrganizationSlugInUse) {
		t.Fatalf("duplicate slug error = %v", err)
	}
}

type memberFixture struct {
	pool      *pgxpool.Pool
	queries   *db.Queries
	tokenKey  []byte
	owner     org.ManagingMember
	admin     org.ManagingMember
	developer org.ManagingMember
	viewer    org.ManagingMember
}

func TestMemberRemovalRevokesOnlyItsLoginSessionsAndRollsBackFailure(t *testing.T) {
	f := newMemberFixture(t)
	otherOrg := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other')`, otherOrg)
	for _, scope := range []any{f.owner.OrgID, nil, otherOrg} {
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO login_sessions(id,user_id,org_id,token_hash,expires_at) VALUES($1,$2,$3,$4,now()+interval '1 hour')`, id, f.developer.UserID, scope, []byte(id.String()))
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_member_session_revocation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected session revocation failure'; END $$;
 CREATE TRIGGER reject_member_session_revocation BEFORE UPDATE ON login_sessions FOR EACH STATEMENT EXECUTE FUNCTION reject_member_session_revocation()`)
	if err := identity.RemoveMember(t.Context(), f.pool, f.owner, f.developer.UserID); err == nil {
		t.Fatal("revocation failure committed member removal")
	}
	var intact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT disabled_at IS NULL AND (SELECT count(*) FROM login_sessions WHERE user_id=$2 AND revoked_at IS NULL)=3 FROM org_members WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.developer.UserID).Scan(&intact); err != nil || !intact {
		t.Fatalf("member or sessions changed after failed removal: %v %v", intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_member_session_revocation ON login_sessions; DROP FUNCTION reject_member_session_revocation()`)
	if err := identity.RemoveMember(t.Context(), f.pool, f.owner, f.developer.UserID); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.pool.QueryRow(t.Context(), `SELECT disabled_at IS NOT NULL AND (SELECT bool_and((revoked_at IS NULL)=(org_id IS NOT DISTINCT FROM $3::uuid)) FROM login_sessions WHERE user_id=$2) FROM org_members WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.developer.UserID, otherOrg).Scan(&correct); err != nil || !correct {
		t.Fatalf("member removal changed the revocation scope: %v %v", correct, err)
	}
}

func TestInitialOrganizationRollsBackFailedAdministratorGrant(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	user := insertUser(t, database.Pool, "Owner", "owner@example.test")
	dbtest.MustExec(t, t.Context(), database.Pool, `CREATE FUNCTION reject_initial_admin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected administrator grant failure'; END $$;
 CREATE TRIGGER reject_initial_admin BEFORE UPDATE ON users FOR EACH STATEMENT EXECUTE FUNCTION reject_initial_admin()`)
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "initial", OwnerUserID: user, InitialSetup: true}); err == nil {
		t.Fatal("failed administrator grant created an organization")
	}
	var intact bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT NOT admin AND NOT EXISTS(SELECT 1 FROM organizations) AND NOT EXISTS(SELECT 1 FROM org_members) FROM users WHERE id=$1`, user).Scan(&intact); err != nil || !intact {
		t.Fatalf("initial setup did not roll back: %v %v", intact, err)
	}
}

func newMemberFixture(t *testing.T) memberFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	orgID := uuid.NewV7()
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Members", Slug: "members",
	}); err != nil {
		t.Fatal(err)
	}
	member := func(name string, email string, role db.OrgMemberRole) org.ManagingMember {
		userID := insertUser(t, database.Pool, name, email)
		if _, err := queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{
			OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: role,
		}); err != nil {
			t.Fatal(err)
		}
		return org.ManagingMember{Actor: audit.System("test-fixture"), OrgID: orgID, UserID: userID, Role: auth.Role(role)}
	}
	keys, err := auth.NewKeys(bytes.Repeat([]byte{7}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return memberFixture{
		pool:      database.Pool,
		queries:   queries,
		tokenKey:  keys.Invitation,
		owner:     member("Owner", "owner@example.test", db.OrgMemberRoleOwner),
		admin:     member("Admin", "admin@example.test", db.OrgMemberRoleAdmin),
		developer: member("Developer", "developer@example.test", db.OrgMemberRoleDeveloper),
		viewer:    member("Viewer", "viewer@example.test", db.OrgMemberRoleViewer),
	}
}

func insertUser(t *testing.T, pool *pgxpool.Pool, name string, email string) uuid.UUID {
	t.Helper()
	userID := uuid.NewV7()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)`, userID, name, email); err != nil {
		t.Fatal(err)
	}
	return userID
}

func TestAcceptInvitationPostgres(t *testing.T) {
	fixture := newMemberFixture(t)
	ctx := t.Context()
	q := fixture.queries
	invite := func(email string, role string) org.PendingInvitation {
		t.Helper()
		_, rawToken, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, fixture.owner, org.InvitationInput{Emails: []string{email}, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		tokenHash, err := auth.HashToken(fixture.tokenKey, rawToken)
		if err != nil {
			t.Fatal(err)
		}
		invitation, err := org.PendingInvitationByTokenHash(ctx, q, tokenHash)
		if err != nil {
			t.Fatal(err)
		}
		byID, err := org.PendingInvitationByID(ctx, q, invitation.ID)
		if err != nil || byID != invitation {
			t.Fatalf("invitation by id = %+v, err = %v", byID, err)
		}
		return invitation
	}
	activeSessions := func(userID uuid.UUID) int {
		t.Helper()
		var count int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM login_sessions WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	session := func(userID uuid.UUID) {
		t.Helper()
		if _, err := fixture.pool.Exec(ctx, `INSERT INTO login_sessions (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`, uuid.NewV7(), userID, uuid.NewV7().String()); err != nil {
			t.Fatal(err)
		}
	}

	// The membership write satisfies the deferred invitation acceptance
	// constraint, so acceptance runs on a transaction.
	accept := func(invitation org.PendingInvitation, userID uuid.UUID, displayName string) error {
		return db.RunTx(ctx, fixture.pool, func(tx pgx.Tx) error {
			return identity.AcceptInvitation(ctx, tx, audit.VerifiedLogin(userID, "test-provider"), invitation, userID, displayName)
		})
	}
	if _, err := org.PendingInvitationByTokenHash(ctx, q, []byte("unknown")); !errors.Is(err, org.ErrInvitationNotFound) {
		t.Fatalf("unknown invitation error = %v", err)
	}
	joiner := insertUser(t, fixture.pool, "Joiner", "joiner@example.test")
	session(joiner)
	invitation := invite("joiner@example.test", "developer")
	if err := accept(invitation, joiner, "Joiner"); err != nil {
		t.Fatal(err)
	}
	member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: pgvalue.UUID(fixture.owner.OrgID), UserID: pgvalue.UUID(joiner)})
	if err != nil || member.DisabledAt.Valid || member.Role != db.OrgMemberRoleDeveloper || member.DisplayName.String != "Joiner" {
		t.Fatalf("member = %+v, err = %v", member, err)
	}
	if activeSessions(joiner) != 0 {
		t.Fatal("acceptance kept earlier login sessions")
	}
	if err := accept(invitation, joiner, "Joiner"); !errors.Is(err, org.ErrAlreadyMember) {
		t.Fatalf("active member error = %v", err)
	}

	// An accepted or revoked invitation cannot be accepted again, and a
	// rejected acceptance leaves sessions alone.
	outsider := insertUser(t, fixture.pool, "Outsider", "outsider@example.test")
	session(outsider)
	if err := accept(invitation, outsider, "Outsider"); !errors.Is(err, org.ErrInvitationNotFound) {
		t.Fatalf("accepted invitation error = %v", err)
	}
	if activeSessions(outsider) != 1 {
		t.Fatal("rejected acceptance revoked sessions")
	}

	// A disabled membership is re-enabled with the invited role.
	if _, err := fixture.pool.Exec(ctx, `UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2`, fixture.owner.OrgID, joiner); err != nil {
		t.Fatal(err)
	}
	reinvitation := invite("second@example.test", "viewer")
	if err := accept(reinvitation, joiner, ""); err != nil {
		t.Fatal(err)
	}
	if member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: pgvalue.UUID(fixture.owner.OrgID), UserID: pgvalue.UUID(joiner)}); err != nil || member.DisabledAt.Valid || member.Role != db.OrgMemberRoleViewer {
		t.Fatalf("re-enabled member = %+v, err = %v", member, err)
	}
	var newGrant bool
	if err := fixture.pool.QueryRow(ctx, `SELECT NOT (details ? 'old_role') AND details->>'new_role'='viewer' AND actor_kind='verified_login' AND target_user_id=$2 FROM audit_events WHERE action='invitation.accepted' AND target_id=$1`, reinvitation.ID, joiner).Scan(&newGrant); err != nil || !newGrant {
		t.Fatal("reinvitation recorded disabled membership as active authority", newGrant, err)
	}
}

func TestMemberAuditFactsNoOpAndRollback(t *testing.T) {
	f := newMemberFixture(t)
	manager := f.owner
	manager.Actor = audit.Actor{Kind: "login", UserID: manager.UserID, CredentialID: uuid.NewV7()}
	if _, err := org.UpdateMemberRole(t.Context(), f.pool, manager, f.developer.UserID, "viewer", "developer"); err != nil {
		t.Fatal(err)
	}
	if _, err := org.UpdateMemberRole(t.Context(), f.pool, manager, f.developer.UserID, "viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='member.role_changed' AND actor_user_id=$1 AND actor_credential_id=$2 AND org_id=$3 AND target_id=$4 AND details->>'old_role'='developer' AND details->>'new_role'='viewer'`, manager.UserID, manager.Actor.CredentialID, manager.OrgID, f.developer.UserID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("role fact count=%d err=%v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_member_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_member_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_member_audit()`)
	if _, err := org.UpdateMemberRole(t.Context(), f.pool, manager, f.developer.UserID, "developer", "viewer"); err == nil {
		t.Fatal("audit failure allowed role change")
	}
	if err := identity.RemoveMember(t.Context(), f.pool, manager, f.developer.UserID); err == nil {
		t.Fatal("audit failure allowed removal")
	}
	var intact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT role='viewer' AND disabled_at IS NULL FROM org_members WHERE org_id=$1 AND user_id=$2`, manager.OrgID, f.developer.UserID).Scan(&intact); err != nil || !intact {
		t.Fatalf("membership failed rollback: %v %v", intact, err)
	}
	if _, _, err := inviteOne(t.Context(), f.pool, f.tokenKey, manager, org.InvitationInput{Emails: []string{"audit@example.test"}, Role: "developer"}); err == nil {
		t.Fatal("audit failure allowed invitation")
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM invitations WHERE invitee_email='audit@example.test'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invitation leaked: %d %v", count, err)
	}
}

// inviteOne invites a single address and returns its invitation and the raw
// token delivered for it, or the error that refused the request or the address.
func inviteOne(ctx context.Context, database db.TxBeginner, tokenKey []byte, inviter org.ManagingMember, input org.InvitationInput) (db.Invitation, string, error) {
	var delivered org.NewInvitation
	results, err := org.CreateInvitations(ctx, database, tokenKey, inviter, input, func(_ context.Context, invitations []org.NewInvitation) error {
		delivered = invitations[0]
		return nil
	})
	if err != nil {
		return db.Invitation{}, "", err
	}
	if results[0].Err != nil {
		return db.Invitation{}, "", results[0].Err
	}
	return delivered.Invitation, delivered.RawToken, nil
}

// recordDelivery delivers nothing and records each batch it was given.
func recordDelivery(batches *[][]org.NewInvitation) org.DeliverInvitations {
	return func(_ context.Context, invitations []org.NewInvitation) error {
		*batches = append(*batches, invitations)
		return nil
	}
}

func TestCreateInvitationsPostgresDeliversOneBatchAndRefusesAddressesOnTheirOwn(t *testing.T) {
	fixture := newMemberFixture(t)
	ctx := t.Context()
	admin := fixture.admin
	if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"old@example.test"}, Role: "viewer"}); err != nil {
		t.Fatal(err)
	}

	var batches [][]org.NewInvitation
	results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{
		Emails: []string{" First@Example.test", "not-an-address", "developer@example.test", "first@example.test", "old@example.test", "second@example.test", "not-an-address"},
		Role:   "developer",
	}, recordDelivery(&batches))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		email string
		err   error
	}{
		{"first@example.test", nil},
		{"not-an-address", nil},
		{"developer@example.test", org.ErrInvitationActiveMember},
		{"old@example.test", org.ErrInvitationPending},
		{"second@example.test", nil},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d distinct addresses", results, len(want))
	}
	for i, expected := range want {
		result := results[i]
		if result.Email != expected.email {
			t.Fatalf("result %d email = %q, want %q", i, result.Email, expected.email)
		}
		switch {
		case expected.email == "not-an-address":
			var input org.InputError
			if !errors.As(result.Err, &input) {
				t.Fatalf("invalid address result = %+v", result)
			}
		case expected.err != nil:
			if !errors.Is(result.Err, expected.err) {
				t.Fatalf("%s result = %+v, want %v", expected.email, result, expected.err)
			}
		default:
			if result.Err != nil {
				t.Fatalf("%s result = %+v", expected.email, result)
			}
			var facts int
			if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events JOIN invitations ON invitations.id = audit_events.target_id WHERE action='invitation.created' AND invitee_email=$1 AND role='developer' AND audit_events.org_id=$2 AND details->>'new_role'='developer'`, expected.email, admin.OrgID).Scan(&facts); err != nil || facts != 1 {
				t.Fatalf("%s audit facts = %d, err = %v", expected.email, facts, err)
			}
		}
	}
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("delivered batches = %+v, want one batch of the two new invitations", batches)
	}
	for i, i2 := range []int{0, 4} {
		delivered := batches[0][i]
		if delivered.Invitation.InviteeEmail != results[i2].Email || delivered.Invitation.Role != db.OrgMemberRoleDeveloper || delivered.RawToken == "" {
			t.Fatalf("delivered %d = %+v, want %s", i, delivered, results[i2].Email)
		}
		tokenHash, err := auth.HashToken(fixture.tokenKey, delivered.RawToken)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := org.PendingInvitationByTokenHash(ctx, fixture.queries, tokenHash)
		if err != nil || pending.InviteeEmail != results[i2].Email {
			t.Fatalf("delivered token resolves to %+v, err = %v", pending, err)
		}
	}

	t.Run("invitations are written and delivered in address order", func(t *testing.T) {
		var ordered [][]org.NewInvitation
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"zed@example.test", "amy@example.test", "mia@example.test"}, Role: "viewer"}, recordDelivery(&ordered))
		if err != nil || len(results) != 3 || results[0].Email != "zed@example.test" || results[1].Email != "amy@example.test" {
			t.Fatalf("results = %+v, err = %v, want request order", results, err)
		}
		if len(ordered) != 1 || len(ordered[0]) != 3 {
			t.Fatalf("batches = %+v", ordered)
		}
		for i, want := range []string{"amy@example.test", "mia@example.test", "zed@example.test"} {
			if got := ordered[0][i].Invitation.InviteeEmail; got != want {
				t.Fatalf("delivered %d = %s, want %s", i, got, want)
			}
		}
	})

	t.Run("nothing to deliver skips delivery", func(t *testing.T) {
		var none [][]org.NewInvitation
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"first@example.test", "bad"}, Role: "viewer"}, recordDelivery(&none))
		if err != nil || len(results) != 2 || results[0].Err == nil || results[1].Err == nil || len(none) != 0 {
			t.Fatalf("results = %+v, batches = %d, err = %v", results, len(none), err)
		}
	})

	t.Run("a failed delivery rolls every invitation of the request back", func(t *testing.T) {
		sendErr := errors.New("provider unavailable")
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{
			Emails: []string{"third@example.test", "developer@example.test", "fourth@example.test"},
			Role:   "viewer",
		}, func(context.Context, []org.NewInvitation) error { return sendErr })
		if err != nil {
			t.Fatal(err)
		}
		for i, address := range []string{"third@example.test", "fourth@example.test"} {
			result := results[i*2]
			if result.Email != address || !errors.Is(result.Err, org.ErrInvitationNotDelivered) || !errors.Is(result.Err, sendErr) {
				t.Fatalf("%s result = %+v", address, result)
			}
		}
		if !errors.Is(results[1].Err, org.ErrInvitationActiveMember) {
			t.Fatalf("refused address result = %+v", results[1])
		}
		var invitations, facts int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email IN ('third@example.test', 'fourth@example.test')`).Scan(&invitations); err != nil || invitations != 0 {
			t.Fatalf("invitations after failed delivery = %d, err = %v", invitations, err)
		}
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='invitation.created' AND target_id NOT IN (SELECT id FROM invitations)`).Scan(&facts); err != nil || facts != 0 {
			t.Fatalf("audit facts after failed delivery = %d, err = %v", facts, err)
		}
	})

	t.Run("the request is rejected as a whole", func(t *testing.T) {
		tooMany := make([]string, org.MaxInvitationsPerRequest+1)
		for i := range tooMany {
			tooMany[i] = fmt.Sprintf("bulk-%d@example.test", i)
		}
		var rejected [][]org.NewInvitation
		var input org.InputError
		for name, request := range map[string]org.InvitationInput{
			"no addresses": {Role: "viewer"},
			"over limit":   {Emails: tooMany, Role: "viewer"},
			"invalid role": {Emails: []string{"role@example.test"}, Role: "guest"},
		} {
			if _, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, request, recordDelivery(&rejected)); !errors.As(err, &input) {
				t.Fatalf("%s error = %v, want input error", name, err)
			}
		}
		if _, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"owner-bulk@example.test"}, Role: "owner"}, recordDelivery(&rejected)); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner batch error = %v", err)
		}
		if _, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, fixture.developer, org.InvitationInput{Emails: []string{"dev-bulk@example.test"}, Role: "viewer"}, recordDelivery(&rejected)); !errors.Is(err, org.ErrMemberManagementRequired) {
			t.Fatalf("developer batch error = %v", err)
		}
		var leaked int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email LIKE 'bulk-%' OR invitee_email IN ('role@example.test', 'owner-bulk@example.test', 'dev-bulk@example.test')`).Scan(&leaked); err != nil || leaked != 0 || len(rejected) != 0 {
			t.Fatalf("rejected requests created %d invitations, delivered %d batches, err = %v", leaked, len(rejected), err)
		}
	})
}
