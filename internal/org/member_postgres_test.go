package org_test

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"

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
			if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, principal, org.InvitationInput{Email: "blocked@example.test", Role: "viewer"}); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s invitation error = %v", principal.Role, err)
			}
			if err := org.RevokeInvitation(ctx, q, principal, uuid.NewV7()); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("%s revoke error = %v", principal.Role, err)
			}
			if _, err := org.UpdateMemberRole(ctx, q, principal, fixture.viewer.UserID, "developer", "viewer"); !errors.Is(err, org.ErrMemberManagementRequired) {
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
		if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "next-owner@example.test", Role: "owner"}); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation error = %v", err)
		}
		invitation, rawToken, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "Teammate@Example.test", Role: "developer"})
		if err != nil || rawToken == "" || invitation.InviteeEmail != "teammate@example.test" {
			t.Fatalf("invitation = %+v, token set %t, err = %v", invitation, rawToken != "", err)
		}
		if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "teammate@example.test", Role: "viewer"}); !errors.Is(err, org.ErrInvitationPending) {
			t.Fatalf("duplicate invitation error = %v", err)
		}
		if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "developer@example.test", Role: "viewer"}); !errors.Is(err, org.ErrInvitationActiveMember) {
			t.Fatalf("active member invitation error = %v", err)
		}
		days := 31
		var input org.InputError
		if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "later@example.test", Role: "viewer", ExpiresInDays: &days}); !errors.As(err, &input) {
			t.Fatalf("expiry error = %v, want input error", err)
		}

		ownerInvitation, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, owner, org.InvitationInput{Email: "next-owner@example.test", Role: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := org.CreateInvitation(ctx, q, fixture.tokenKey, admin, org.InvitationInput{Email: "next-owner@example.test", Role: "viewer"}); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin invitation over pending owner invitation error = %v", err)
		}
		ownerInvitationID := pgvalue.MustUUIDValue(ownerInvitation.ID)
		if err := org.RevokeInvitation(ctx, q, admin, ownerInvitationID); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation revoke error = %v", err)
		}
		if err := org.RevokeInvitation(ctx, q, owner, ownerInvitationID); err != nil {
			t.Fatal(err)
		}
		if err := org.RevokeInvitation(ctx, q, owner, ownerInvitationID); !errors.Is(err, org.ErrInvitationNotFound) {
			t.Fatalf("second revoke error = %v", err)
		}
	})

	t.Run("role changes", func(t *testing.T) {
		updated, err := org.UpdateMemberRole(ctx, q, admin, developer.UserID, "viewer", "developer")
		if err != nil || updated.Member.Role != db.OrgMemberRoleViewer || updated.PrimaryEmail.String != "developer@example.test" {
			t.Fatalf("updated = %+v, err = %v", updated, err)
		}
		if _, err := org.UpdateMemberRole(ctx, q, admin, developer.UserID, "developer", "developer"); !errors.Is(err, org.ErrMemberRoleChanged) {
			t.Fatalf("stale expected role error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, q, admin, developer.UserID, "owner", "viewer"); !errors.Is(err, org.ErrOwnerRoleRequired) {
			t.Fatalf("admin promotion to owner error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, q, admin, admin.UserID, "developer", "admin"); !errors.Is(err, org.ErrSelfMemberManagement) {
			t.Fatalf("self demotion error = %v", err)
		}
		if _, err := org.UpdateMemberRole(ctx, q, owner, owner.UserID, "admin", "owner"); !errors.Is(err, org.ErrLastActiveOwner) {
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
	created, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Slug: "Acme", OwnerUserID: userID, InitialSetup: true})
	if err != nil || created.Slug != "acme" || created.Name != "acme" {
		t.Fatalf("organization = %+v, err = %v", created, err)
	}
	var admin bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT admin FROM users WHERE id = $1`, userID).Scan(&admin); err != nil || !admin {
		t.Fatalf("initial owner admin = %t, err = %v", admin, err)
	}
	member, err := queries.GetOrgMemberForManagement(t.Context(), db.GetOrgMemberForManagementParams{OrgID: created.ID, UserID: pgvalue.UUID(userID)})
	if err != nil || member.DisabledAt.Valid || member.UserDisabledAt.Valid || member.Role != db.OrgMemberRoleOwner {
		t.Fatalf("owner member = %+v, err = %v", member, err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Slug: "second", OwnerUserID: userID, InitialSetup: true}); !errors.Is(err, org.ErrOrganizationExists) {
		t.Fatalf("second setup error = %v", err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Slug: "acme", OwnerUserID: userID}); !errors.Is(err, org.ErrOrganizationSlugInUse) {
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
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Slug: "initial", OwnerUserID: user, InitialSetup: true}); err == nil {
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
		return org.ManagingMember{OrgID: orgID, UserID: userID, Role: auth.Role(role)}
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
		_, rawToken, err := org.CreateInvitation(ctx, q, fixture.tokenKey, fixture.owner, org.InvitationInput{Email: email, Role: role})
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
			return identity.AcceptInvitation(ctx, db.New(tx), invitation, userID, displayName)
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
	if err != nil || member.DisabledAt.Valid || member.UserDisabledAt.Valid || member.Role != db.OrgMemberRoleDeveloper || member.DisplayName.String != "Joiner" {
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

	// A disabled membership is re-enabled with the invited role; a disabled
	// user with an active membership is reported as disabled.
	if _, err := fixture.pool.Exec(ctx, `UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2`, fixture.owner.OrgID, joiner); err != nil {
		t.Fatal(err)
	}
	if err := accept(invite("second@example.test", "viewer"), joiner, ""); err != nil {
		t.Fatal(err)
	}
	if member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: pgvalue.UUID(fixture.owner.OrgID), UserID: pgvalue.UUID(joiner)}); err != nil || member.DisabledAt.Valid || member.UserDisabledAt.Valid || member.Role != db.OrgMemberRoleViewer {
		t.Fatalf("re-enabled member = %+v, err = %v", member, err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, joiner); err != nil {
		t.Fatal(err)
	}
	if err := accept(invite("third@example.test", "viewer"), joiner, ""); !errors.Is(err, org.ErrUserDisabled) {
		t.Fatalf("disabled user error = %v", err)
	}
}
