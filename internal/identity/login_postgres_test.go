package identity

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestSignInPostgres(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()

	admin := ExternalIdentity{Provider: "github", Subject: "1", DisplayName: "admin", Email: "admin@example.test", EmailVerified: true}
	raw, err := SignIn(ctx, fixture.queries, fixture.cfg, admin)
	if err != nil {
		t.Fatal(err)
	}
	adminID, isAdmin := fixture.userByEmail(t, "admin@example.test")
	if !isAdmin {
		t.Fatal("verified configured address did not become an administrator")
	}
	var storedHash []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT token_hash FROM auth_sessions WHERE user_id = $1`, adminID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if want, _ := auth.HashToken(fixture.keys.Session, raw); !bytes.Equal(storedHash, want) {
		t.Fatal("login session was not hashed with the session key")
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, raw)
	if err != nil || principal.UserID != adminID || principal.Kind != auth.ActorKindSession || !principal.Admin || principal.Role != "" {
		t.Fatalf("principal = %+v, err = %v", principal, err)
	}

	unverified := ExternalIdentity{Provider: "github", Subject: "2", DisplayName: "unverified", Email: "admin2@example.test"}
	unverifiedConfig := NewConfig(fixture.keys, Lifetimes{}, []string{"admin2@example.test"})
	if _, err := SignIn(ctx, fixture.queries, unverifiedConfig, unverified); err != nil {
		t.Fatal(err)
	}
	var unverifiedAdmin bool
	if err := fixture.pool.QueryRow(ctx, `SELECT users.admin FROM users JOIN auth_identities ON auth_identities.user_id = users.id WHERE auth_identities.subject = '2'`).Scan(&unverifiedAdmin); err != nil || unverifiedAdmin {
		t.Fatalf("unverified configured address admin = %t, err = %v", unverifiedAdmin, err)
	}

	fixture.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, adminID)
	if _, err := SignIn(ctx, fixture.queries, fixture.cfg, admin); !errors.Is(err, ErrInactiveMember) {
		t.Fatalf("disabled sign-in error = %v", err)
	}
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled user session error = %v", err)
	}
}

func TestSignInWithInvitationPostgres(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	orgID := fixture.organization(t, "invited")
	invitation, rawToken := fixture.invitation(t, orgID, "invitee@example.test", "developer")

	resolved, tokenHash, err := ResolveInvitation(ctx, fixture.queries, fixture.cfg, rawToken)
	if err != nil || resolved.OrgID != orgID || resolved.InviteeEmail != "invitee@example.test" {
		t.Fatalf("resolved = %+v, err = %v", resolved, err)
	}
	if want, _ := auth.HashToken(fixture.keys.Invitation, rawToken); !bytes.Equal(tokenHash, want) {
		t.Fatal("invitation was not resolved with the invitation key")
	}
	if _, _, err := ResolveInvitation(ctx, fixture.queries, fixture.cfg, "unknown"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown invitation error = %v", err)
	}

	other := ExternalIdentity{Provider: "github", Subject: "other", DisplayName: "other", Email: "other@example.test", EmailVerified: true}
	if _, err := SignInWithInvitation(ctx, fixture.pool, fixture.cfg, tokenHash, other); !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("wrong account error = %v", err)
	}
	unverified := ExternalIdentity{Provider: "github", Subject: "invitee", DisplayName: "invitee", Email: "invitee@example.test"}
	if _, err := SignInWithInvitation(ctx, fixture.pool, fixture.cfg, tokenHash, unverified); !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("unverified invitee error = %v", err)
	}

	// The invitee verifies the invited address as a secondary address and
	// already holds a login session, which acceptance revokes.
	invitee := ExternalIdentity{
		Provider: "github", Subject: "invitee", DisplayName: "Invitee",
		Email: "primary@example.test", EmailVerified: true, VerifiedEmails: []string{"primary@example.test", "Invitee@Example.test"},
	}
	earlier, err := SignIn(ctx, fixture.queries, fixture.cfg, invitee)
	if err != nil {
		t.Fatal(err)
	}
	inviteeID, _ := fixture.userByEmail(t, "primary@example.test")
	raw, err := SignInWithInvitation(ctx, fixture.pool, fixture.cfg, tokenHash, invitee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, earlier); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("earlier session error = %v", err)
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, raw)
	if err != nil || principal.UserID != inviteeID || principal.OrgID != orgID || principal.Role != auth.RoleDeveloper {
		t.Fatalf("invitation principal = %+v, err = %v", principal, err)
	}
	// Acceptance revokes sessions after writing the membership; the session it
	// issues afterwards is the only one that survives.
	if active := fixture.activeSessions(t, inviteeID); active != 1 {
		t.Fatalf("active sessions after acceptance = %d, want 1", active)
	}
	var acceptedBy uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT accepted_by_user_id FROM invitations WHERE id = $1`, invitation.ID).Scan(&acceptedBy); err != nil || acceptedBy != inviteeID {
		t.Fatalf("accepted by = %s, err = %v", acceptedBy, err)
	}
	if _, err := SignInWithInvitation(ctx, fixture.pool, fixture.cfg, tokenHash, invitee); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reused invitation error = %v", err)
	}

	// A second invitation to an address the member also verifies is rejected
	// without accepting it.
	_, secondToken := fixture.invitation(t, orgID, "second@example.test", "viewer")
	_, secondHash, err := ResolveInvitation(ctx, fixture.queries, fixture.cfg, secondToken)
	if err != nil {
		t.Fatal(err)
	}
	invitee.VerifiedEmails = append(invitee.VerifiedEmails, "second@example.test")
	if _, err := SignInWithInvitation(ctx, fixture.pool, fixture.cfg, secondHash, invitee); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("already member error = %v", err)
	}
	if fixture.activeSessions(t, inviteeID) != 1 {
		t.Fatal("rejected acceptance revoked sessions")
	}

	// A disabled membership is re-enabled with the invited role.
	fixture.exec(t, `UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2`, orgID, inviteeID)
	raw, err = SignInWithInvitation(ctx, fixture.pool, fixture.cfg, secondHash, invitee)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, raw)
	if err != nil || principal.OrgID != orgID || principal.Role != auth.RoleViewer {
		t.Fatalf("re-enabled principal = %+v, err = %v", principal, err)
	}
}

func TestLoginSessionPostgresFollowsSelectedMembership(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Developer", "")
	first, invited := fixture.organization(t, "first"), fixture.organization(t, "invited")
	fixture.exec(t, `INSERT INTO org_members (org_id, user_id, role, created_at) VALUES ($1, $3, 'owner', now() - interval '1 day'), ($2, $3, 'viewer', now())`, first, invited, userID)

	pinned := fixture.session(t, userID, invited)
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, pinned)
	if err != nil || principal.OrgID != invited || principal.Role != auth.RoleViewer {
		t.Fatalf("pinned principal = %+v, err = %v", principal, err)
	}
	unpinned := fixture.session(t, userID, uuid.Nil())
	principal, err = AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, unpinned)
	if err != nil || principal.OrgID != first || principal.Role != auth.RoleOwner {
		t.Fatalf("unpinned principal = %+v, err = %v", principal, err)
	}

	// A disabled pinned membership invalidates the session instead of falling
	// through to another organization.
	fixture.exec(t, `UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2`, invited, userID)
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, pinned); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled pinned membership error = %v", err)
	}
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, unpinned); err != nil {
		t.Fatalf("unpinned session lost its first membership: %v", err)
	}

	// Authentication extends the session's lifetime.
	tokenHash, _ := auth.HashToken(fixture.keys.Session, unpinned)
	fixture.exec(t, `UPDATE auth_sessions SET expires_at = now() + interval '1 minute' WHERE token_hash = $1`, tokenHash)
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, unpinned); err != nil {
		t.Fatal(err)
	}
	var expiresAt time.Time
	if err := fixture.pool.QueryRow(ctx, `SELECT expires_at FROM auth_sessions WHERE token_hash = $1`, tokenHash).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if time.Until(expiresAt) < fixture.cfg.Lifetimes().Session-time.Minute {
		t.Fatalf("session expiry was not extended: %s", expiresAt)
	}

	if err := RevokeLoginSession(ctx, fixture.queries, fixture.cfg, unpinned); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, unpinned); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked session error = %v", err)
	}
	if err := RevokeLoginSession(ctx, fixture.queries, fixture.cfg, " "); err != nil {
		t.Fatalf("empty token revocation error = %v", err)
	}
	if _, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, ""); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("empty token error = %v", err)
	}
}

func TestLoadAccountPostgres(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Newcomer", "")
	account, err := LoadAccount(ctx, fixture.queries, auth.Actor{UserID: userID, Kind: auth.ActorKindSession})
	if err != nil || account.DisplayName != "Newcomer" || account.OrganizationExists {
		t.Fatalf("account = %+v, err = %v", account, err)
	}
	orgID := fixture.organization(t, "account")
	fixture.member(t, orgID, userID, db.OrgMemberRoleOwner)
	account, err = LoadAccount(ctx, fixture.queries, auth.Actor{UserID: userID, OrgID: orgID, Kind: auth.ActorKindSession})
	if err != nil || account.OrgSlug != "account" || account.HasProjects {
		t.Fatalf("org account = %+v, err = %v", account, err)
	}
	account, err = LoadAccount(ctx, fixture.queries, auth.Actor{UserID: userID, Kind: auth.ActorKindSession})
	if err != nil || !account.OrganizationExists {
		t.Fatalf("account without selected organization = %+v, err = %v", account, err)
	}
	if _, err := LoadAccount(ctx, fixture.queries, auth.Actor{UserID: uuid.NewV7(), Kind: auth.ActorKindSession}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user error = %v", err)
	}
}
