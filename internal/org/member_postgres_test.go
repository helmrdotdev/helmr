package org

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

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
		for _, principal := range []ManagingMember{developer, fixture.viewer} {
			if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, principal, InvitationInput{Email: "blocked@example.test", Role: "viewer"}); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s invitation error = %v", principal.Role, err)
			}
			if err := RevokeInvitation(ctx, q, principal, uuid.NewV7()); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s revoke error = %v", principal.Role, err)
			}
			if _, err := UpdateMemberRole(ctx, q, principal, fixture.viewer.UserID, "developer", "viewer"); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s role change error = %v", principal.Role, err)
			}
			if err := RemoveMember(ctx, q, principal, fixture.viewer.UserID); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s removal error = %v", principal.Role, err)
			}
			if _, err := ListMembers(ctx, q, principal); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s member list error = %v", principal.Role, err)
			}
			if _, _, err := ListInvitations(ctx, q, principal, 10, nil); !errors.Is(err, ErrMemberManagementRequired) {
				t.Fatalf("%s invitation list error = %v", principal.Role, err)
			}
		}
		var blocked int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email = 'blocked@example.test'`).Scan(&blocked); err != nil || blocked != 0 {
			t.Fatalf("blocked invitations = %d, err = %v", blocked, err)
		}
	})

	t.Run("invitations", func(t *testing.T) {
		if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "next-owner@example.test", Role: "owner"}); !errors.Is(err, ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation error = %v", err)
		}
		invitation, rawToken, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "Teammate@Example.test", Role: "developer"})
		if err != nil || rawToken == "" || invitation.InviteeEmail != "teammate@example.test" {
			t.Fatalf("invitation = %+v, token set %t, err = %v", invitation, rawToken != "", err)
		}
		if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "teammate@example.test", Role: "viewer"}); !errors.Is(err, ErrInvitationPending) {
			t.Fatalf("duplicate invitation error = %v", err)
		}
		if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "developer@example.test", Role: "viewer"}); !errors.Is(err, ErrInvitationActiveMember) {
			t.Fatalf("active member invitation error = %v", err)
		}
		days := 31
		var input InputError
		if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "later@example.test", Role: "viewer", ExpiresInDays: &days}); !errors.As(err, &input) {
			t.Fatalf("expiry error = %v, want input error", err)
		}

		ownerInvitation, _, err := CreateInvitation(ctx, q, fixture.tokenKey, owner, InvitationInput{Email: "next-owner@example.test", Role: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := CreateInvitation(ctx, q, fixture.tokenKey, admin, InvitationInput{Email: "next-owner@example.test", Role: "viewer"}); !errors.Is(err, ErrOwnerRoleRequired) {
			t.Fatalf("admin invitation over pending owner invitation error = %v", err)
		}
		ownerInvitationID := pgvalue.MustUUIDValue(ownerInvitation.ID)
		if err := RevokeInvitation(ctx, q, admin, ownerInvitationID); !errors.Is(err, ErrOwnerRoleRequired) {
			t.Fatalf("admin owner invitation revoke error = %v", err)
		}
		if err := RevokeInvitation(ctx, q, owner, ownerInvitationID); err != nil {
			t.Fatal(err)
		}
		if err := RevokeInvitation(ctx, q, owner, ownerInvitationID); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("second revoke error = %v", err)
		}
	})

	t.Run("role changes", func(t *testing.T) {
		updated, err := UpdateMemberRole(ctx, q, admin, developer.UserID, "viewer", "developer")
		if err != nil || updated.Member.Role != db.OrgMemberRoleViewer || updated.PrimaryEmail.String != "developer@example.test" {
			t.Fatalf("updated = %+v, err = %v", updated, err)
		}
		if _, err := UpdateMemberRole(ctx, q, admin, developer.UserID, "developer", "developer"); !errors.Is(err, ErrMemberRoleChanged) {
			t.Fatalf("stale expected role error = %v", err)
		}
		if _, err := UpdateMemberRole(ctx, q, admin, developer.UserID, "owner", "viewer"); !errors.Is(err, ErrOwnerRoleRequired) {
			t.Fatalf("admin promotion to owner error = %v", err)
		}
		if _, err := UpdateMemberRole(ctx, q, admin, admin.UserID, "developer", "admin"); !errors.Is(err, ErrSelfMemberManagement) {
			t.Fatalf("self demotion error = %v", err)
		}
		if _, err := UpdateMemberRole(ctx, q, owner, owner.UserID, "admin", "owner"); !errors.Is(err, ErrLastActiveOwner) {
			t.Fatalf("last owner demotion error = %v", err)
		}
	})

	t.Run("removal", func(t *testing.T) {
		if err := RemoveMember(ctx, q, admin, admin.UserID); !errors.Is(err, ErrSelfMemberRemoval) {
			t.Fatalf("self removal error = %v", err)
		}
		if err := RemoveMember(ctx, q, admin, owner.UserID); !errors.Is(err, ErrOwnerRoleRequired) {
			t.Fatalf("admin owner removal error = %v", err)
		}
		if err := RemoveMember(ctx, q, admin, developer.UserID); err != nil {
			t.Fatal(err)
		}
		if err := RemoveMember(ctx, q, admin, developer.UserID); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("second removal error = %v", err)
		}
		members, err := ListMembers(ctx, q, owner)
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
	userID := insertUser(t, database.Pool, "Founder", "founder@example.test")
	created, err := CreateOrganization(t.Context(), database.Pool, OrganizationInput{Slug: "Acme", OwnerUserID: userID, InitialSetup: true})
	if err != nil || created.Slug != "acme" || created.Name != "acme" {
		t.Fatalf("organization = %+v, err = %v", created, err)
	}
	var admin bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT admin FROM users WHERE id = $1`, userID).Scan(&admin); err != nil || !admin {
		t.Fatalf("initial owner admin = %t, err = %v", admin, err)
	}
	member, err := queries.GetOrgMemberForManagement(t.Context(), db.GetOrgMemberForManagementParams{OrgID: created.ID, UserID: pgvalue.UUID(userID)})
	if err != nil || member.Role != db.OrgMemberRoleOwner {
		t.Fatalf("owner member = %+v, err = %v", member, err)
	}
	if _, err := CreateOrganization(t.Context(), database.Pool, OrganizationInput{Slug: "second", OwnerUserID: userID, InitialSetup: true}); !errors.Is(err, ErrOrganizationExists) {
		t.Fatalf("second setup error = %v", err)
	}
	if _, err := CreateOrganization(t.Context(), database.Pool, OrganizationInput{Slug: "acme", OwnerUserID: userID}); !errors.Is(err, ErrOrganizationSlugInUse) {
		t.Fatalf("duplicate slug error = %v", err)
	}
}

type memberFixture struct {
	pool      *pgxpool.Pool
	queries   *db.Queries
	tokenKey  []byte
	owner     ManagingMember
	admin     ManagingMember
	developer ManagingMember
	viewer    ManagingMember
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
	member := func(name string, email string, role db.OrgMemberRole) ManagingMember {
		userID := insertUser(t, database.Pool, name, email)
		if _, err := queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{
			OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: role,
		}); err != nil {
			t.Fatal(err)
		}
		return ManagingMember{OrgID: orgID, UserID: userID, Role: auth.Role(role)}
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
	invite := func(email string, role string) PendingInvitation {
		t.Helper()
		_, rawToken, err := CreateInvitation(ctx, q, fixture.tokenKey, fixture.owner, InvitationInput{Email: email, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		tokenHash, err := auth.HashToken(fixture.tokenKey, rawToken)
		if err != nil {
			t.Fatal(err)
		}
		invitation, err := PendingInvitationByTokenHash(ctx, q, tokenHash)
		if err != nil {
			t.Fatal(err)
		}
		byID, err := PendingInvitationByID(ctx, q, invitation.ID)
		if err != nil || byID != invitation {
			t.Fatalf("invitation by id = %+v, err = %v", byID, err)
		}
		return invitation
	}
	activeSessions := func(userID uuid.UUID) int {
		t.Helper()
		var count int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	session := func(userID uuid.UUID) {
		t.Helper()
		if _, err := fixture.pool.Exec(ctx, `INSERT INTO auth_sessions (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`, uuid.NewV7(), userID, uuid.NewV7().String()); err != nil {
			t.Fatal(err)
		}
	}

	// The membership write satisfies the deferred invitation acceptance
	// constraint, so acceptance runs on a transaction.
	accept := func(invitation PendingInvitation, userID uuid.UUID, displayName string) error {
		return db.RunTx(ctx, fixture.pool, func(tx pgx.Tx) error {
			return AcceptInvitation(ctx, db.New(tx), invitation, userID, displayName)
		})
	}
	if _, err := PendingInvitationByTokenHash(ctx, q, []byte("unknown")); !errors.Is(err, ErrInvitationNotFound) {
		t.Fatalf("unknown invitation error = %v", err)
	}
	joiner := insertUser(t, fixture.pool, "Joiner", "joiner@example.test")
	session(joiner)
	invitation := invite("joiner@example.test", "developer")
	if err := accept(invitation, joiner, "Joiner"); err != nil {
		t.Fatal(err)
	}
	member, err := activeMember(ctx, q, fixture.owner.OrgID, joiner)
	if err != nil || member.Role != db.OrgMemberRoleDeveloper || member.DisplayName.String != "Joiner" {
		t.Fatalf("member = %+v, err = %v", member, err)
	}
	if activeSessions(joiner) != 0 {
		t.Fatal("acceptance kept earlier login sessions")
	}
	if err := accept(invitation, joiner, "Joiner"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("active member error = %v", err)
	}

	// An accepted or revoked invitation cannot be accepted again, and a
	// rejected acceptance leaves sessions alone.
	outsider := insertUser(t, fixture.pool, "Outsider", "outsider@example.test")
	session(outsider)
	if err := accept(invitation, outsider, "Outsider"); !errors.Is(err, ErrInvitationNotFound) {
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
	if member, err := activeMember(ctx, q, fixture.owner.OrgID, joiner); err != nil || member.Role != db.OrgMemberRoleViewer {
		t.Fatalf("re-enabled member = %+v, err = %v", member, err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, joiner); err != nil {
		t.Fatal(err)
	}
	if err := accept(invite("third@example.test", "viewer"), joiner, ""); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled user error = %v", err)
	}
}
