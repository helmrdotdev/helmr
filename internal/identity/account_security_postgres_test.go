package identity

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"uuid"
)

func TestAccountRevocationRetiresApprovedAccessAndKeepsCredentials(t *testing.T) {
	f := newIdentityFixture(t)
	c, b := emailChallenge(t, f, "owner@example.test")
	if err := MarkEmailDelivered(t.Context(), f.pool, c); err != nil {
		t.Fatal(err)
	}
	done, err := completeEmail(t, f, c, b)
	if err != nil {
		t.Fatal(err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, done.Token, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := AccountCredentials(t.Context(), f.pool, p)
	if err != nil || len(credentials) != 0 {
		t.Fatal("email offered as removable credential", credentials, err)
	}
	pending, _ := emailChallenge(t, f, "owner@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, pending); err != nil {
		t.Fatal(err)
	}
	org := f.organization(t, "account-revoke")
	f.member(t, org, p.UserID, "member")
	scoped, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, done.Token, org)
	if err != nil {
		t.Fatal(err)
	}
	device, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, scoped, DeviceConsent{OrgID: org.String()}, device.UserCode); err != nil {
		t.Fatal(err)
	}
	if err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, p); err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, done.Token, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("session survived", err)
	}
	// Anonymous sign-in has not acquired account authority. A valid credential
	// may start a new session before or after revoke-all.
	if _, err = completeEmail(t, f, pending, b); err != nil {
		t.Fatal("valid anonymous sign-in was rejected", err)
	}
	if _, err = ExchangeDeviceCode(t.Context(), f.pool, f.cfg, device.DeviceCode); err == nil {
		t.Fatal("approved device survived")
	}
	fresh, _ := emailChallenge(t, f, "owner@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, fresh); err != nil {
		t.Fatal(err)
	}
	renewed, err := completeEmail(t, f, fresh, b)
	if err != nil {
		t.Fatal(err)
	}
	current, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, renewed.Token, uuid.Nil())
	if err != nil || current.UserID != p.UserID || current.IdentityID != p.IdentityID || current.AccessEpoch != p.AccessEpoch+1 {
		t.Fatal("revocation removed method or changed identity", current, err)
	}
}
func TestEmailChangeRetiresOnlyMailboxSource(t *testing.T) {
	f := newIdentityFixture(t)
	external := ExternalIdentity{Provider: "github", Subject: "independent", DisplayName: "Owner", Email: "old@example.test", EmailVerified: true}
	github, err := SignIn(t.Context(), f.pool, f.cfg, external)
	if err != nil {
		t.Fatal(err)
	}
	c, b := emailChallenge(t, f, "old@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, c); err != nil {
		t.Fatal(err)
	}
	emailOnly, err := completeEmail(t, f, c, b)
	if err != nil {
		t.Fatal(err)
	}
	original, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, emailOnly.Token, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, emailOnly.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, emailOnly.Token, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("old rotation token survived", err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, mixed, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	// Retain a separate email-only envelope to prove source-specific revocation.
	c2, _ := emailChallenge(t, f, "old@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, c2); err != nil {
		t.Fatal(err)
	}
	separate, err := completeEmail(t, f, c2, b)
	if err != nil {
		t.Fatal(err)
	}
	change, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "new@example.test", BrowserHash: b, Session: p, EmailChange: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = MarkEmailDelivered(t.Context(), f.pool, change); err != nil {
		t.Fatal(err)
	}
	changed, err := CompleteEmailChallenge(t.Context(), f.pool, f.cfg, change.ID, b, change.Code, mixed, "email_change")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, separate.Token, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("old mailbox source survived", err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, github, uuid.Nil()); err != nil {
		t.Fatal("independent provider revoked", err)
	}
	current, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, changed.Token, uuid.Nil())
	if err != nil || current.SessionID != original.SessionID || current.UserID != original.UserID || current.IdentityID == original.IdentityID {
		t.Fatal(current, err)
	}
	var source uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT identity_id FROM login_sessions WHERE id=$1`, p.SessionID).Scan(&source); err != nil || source != current.IdentityID {
		t.Fatal("current proof was not replaced", source, err)
	}
	var notices int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(DISTINCT recipient) FROM account_security_notifications WHERE event='identity.email_changed' AND recipient IN ('old@example.test','new@example.test')`).Scan(&notices); err != nil || notices != 2 {
		t.Fatal("email change notifications", notices, err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, current, p.IdentityID); err != nil {
		t.Fatal("retained email proof should permit provider removal", err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, github, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("removed provider survived", err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, current, current.IdentityID); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatal("last proof removal", err)
	}
}
