package identity

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"time"
	"uuid"
)

func TestSignInPostgresFailedSessionRecordsNothing(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	returning := ExternalIdentity{Provider: "github", Subject: "returning", DisplayName: "Before", Email: "returning@example.test", EmailVerified: true}
	if _, err := SignIn(ctx, fixture.pool, fixture.cfg, returning); err != nil {
		t.Fatal(err)
	}
	returningID, _ := fixture.userByEmail(t, "returning@example.test")

	restore := fixture.rejectLoginSessions(t)
	first := ExternalIdentity{Provider: "github", Subject: "first", DisplayName: "First", Email: "admin@example.test", EmailVerified: true}
	if _, err := SignIn(ctx, fixture.pool, fixture.cfg, first); err == nil {
		t.Fatal("first sign-in succeeded without a login session")
	}
	var identities, users int
	if err := fixture.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_identities WHERE subject = 'first'), (SELECT count(*) FROM users WHERE primary_email = 'admin@example.test')`).Scan(&identities, &users); err != nil || identities != 0 || users != 0 {
		t.Fatalf("after failed first sign-in identities = %d users = %d, err = %v, want none", identities, users, err)
	}
	renamed := returning
	renamed.DisplayName = "After"
	if _, err := SignIn(ctx, fixture.pool, fixture.cfg, renamed); err == nil {
		t.Fatal("returning sign-in succeeded without a login session")
	}
	var displayName string
	if err := fixture.pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, returningID).Scan(&displayName); err != nil || displayName != "Before" {
		t.Fatalf("display name after failed sign-in = %q, err = %v, want Before", displayName, err)
	}
	restore()

	raw, err := SignIn(ctx, fixture.pool, fixture.cfg, first)
	if err != nil {
		t.Fatal(err)
	}
	firstID, admin := fixture.userByEmail(t, "admin@example.test")
	if principal, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, raw, uuid.Nil()); err != nil || principal.UserID != firstID || admin {
		t.Fatalf("retried principal = %+v admin = %t, err = %v", principal, admin, err)
	}
}

func TestLoadAccountPostgres(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Newcomer", "")
	account, err := LoadAccount(ctx, fixture.queries, auth.Principal{UserID: userID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()})
	if err != nil || account.DisplayName != "Newcomer" || account.OrganizationExists {
		t.Fatalf("account = %+v, err = %v", account, err)
	}
	orgID := fixture.organization(t, "account")
	fixture.member(t, orgID, userID, "owner")
	account, err = LoadAccount(ctx, fixture.queries, auth.Principal{UserID: userID, OrgID: orgID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()})
	if err != nil || account.OrgSlug != "account" || account.HasProjects {
		t.Fatalf("org account = %+v, err = %v", account, err)
	}
	account, err = LoadAccount(ctx, fixture.queries, auth.Principal{UserID: userID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()})
	if err != nil || !account.OrganizationExists {
		t.Fatalf("account without selected organization = %+v, err = %v", account, err)
	}
	if _, err := LoadAccount(ctx, fixture.queries, auth.Principal{UserID: uuid.NewV7(), Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user error = %v", err)
	}
}

func TestSignInPostgres(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := t.Context()
	external := ExternalIdentity{Provider: "github", Subject: "1", DisplayName: "Original", Email: "admin@example.test", EmailVerified: true}
	raw, err := SignIn(ctx, f.pool, f.cfg, external)
	if err != nil {
		t.Fatal(err)
	}
	p, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, uuid.Nil())
	if err != nil || p.OperatorID != uuid.Nil() || p.MemberID != uuid.Nil() {
		t.Fatal(p, err)
	}
	var stored []byte
	if err = f.pool.QueryRow(ctx, `SELECT token_hash FROM login_sessions WHERE id=$1`, p.SessionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	want, _ := auth.HashToken(f.keys.Session, raw)
	if !bytes.Equal(stored, want) {
		t.Fatal("unhashed session")
	}
	external.DisplayName = "Provider changed"
	external.Email = "new@example.test"
	if _, err = SignIn(ctx, f.pool, f.cfg, external); err != nil {
		t.Fatal(err)
	}
	var name, email string
	if err = f.pool.QueryRow(ctx, `SELECT display_name,primary_email FROM users WHERE id=$1`, p.UserID).Scan(&name, &email); err != nil || name != "Original" || email != "admin@example.test" {
		t.Fatal("provider renamed account", name, email, err)
	}
	external.Subject = "2"
	external.Email = "ADMIN@example.test"
	if _, err = SignIn(ctx, f.pool, f.cfg, external); !errors.Is(err, ErrAccountProofRequired) {
		t.Fatal("email collision linked unrelated identity", err)
	}
	external.EmailVerified = false
	external.Email = "unverified@example.test"
	if _, err = SignIn(ctx, f.pool, f.cfg, external); !errors.Is(err, ErrAccountProofRequired) {
		t.Fatal("unverified new account", err)
	}
}
func TestLoginSessionPostgresExplicitOrganizationAndActivity(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := t.Context()
	user := f.user(t, "Member", "")
	a, b := f.organization(t, "first"), f.organization(t, "second")
	f.member(t, a, user, "owner")
	f.member(t, b, user, "member")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, uuid.Nil())
	if err != nil || p.OrgID != uuid.Nil() {
		t.Fatal("implicit organization", p, err)
	}
	scoped, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, b)
	if err != nil || scoped.OrgID != b || scoped.IsOwner {
		t.Fatal(scoped, err)
	}
	f.exec(t, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, scoped.MemberID)
	if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, b); !errors.Is(err, ErrOrganizationAccessDenied) {
		t.Fatal("removed scope", err)
	}
	if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, a); err != nil {
		t.Fatal("other scope revoked", err)
	}
	f.exec(t, `UPDATE login_sessions SET expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, p.SessionID)
	if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var expiry time.Time
	if err = f.pool.QueryRow(ctx, `SELECT expires_at FROM login_sessions WHERE id=$1`, p.SessionID).Scan(&expiry); err != nil || time.Until(expiry) > time.Minute {
		t.Fatal("read refreshed inactivity", expiry, err)
	}
	if err = RecordHumanActivity(ctx, f.pool, p); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT expires_at FROM login_sessions WHERE id=$1`, p.SessionID).Scan(&expiry); err != nil || time.Until(expiry) < 29*24*time.Hour {
		t.Fatal("activity did not extend live session", expiry, err)
	}
	if err = RevokeLoginSession(ctx, f.pool, f.cfg, raw); err != nil {
		t.Fatal(err)
	}
	if err = RecordHumanActivity(ctx, f.pool, p); err == nil {
		t.Fatal("activity resurrected revoked session")
	}
	if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, raw, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	if err = RevokeLoginSession(ctx, f.pool, f.cfg, " "); err != nil {
		t.Fatal(err)
	}
}
