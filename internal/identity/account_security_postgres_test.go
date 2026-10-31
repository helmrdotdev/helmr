package identity

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"uuid"

	"github.com/go-webauthn/webauthn/webauthn"
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
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, scoped, device.UserCode); err != nil {
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
	if err != nil || current.UserID != p.UserID || current.IdentityID != p.IdentityID || current.SessionID == p.SessionID {
		t.Fatal("revocation removed method or changed identity", current, err)
	}
}
func TestEmailChangeSignsOutOtherSessions(t *testing.T) {
	f := newIdentityFixture(t)
	external := ExternalIdentity{Provider: "github", Subject: "independent", DisplayName: "Owner", Email: "old@example.test", EmailVerified: true}
	github, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", uuid.Nil())
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
	mixed, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, emailOnly.Token, original.SessionID)
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
	// Another browser and a CLI login of the same account.
	c2, _ := emailChallenge(t, f, "old@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, c2); err != nil {
		t.Fatal(err)
	}
	separate, err := completeEmail(t, f, c2, b)
	if err != nil {
		t.Fatal(err)
	}
	org := f.organization(t, "email-change")
	f.member(t, org, p.UserID, "member")
	approver, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, separate.Token, org)
	if err != nil {
		t.Fatal(err)
	}
	device, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, approver, device.UserCode); err != nil {
		t.Fatal(err)
	}
	cli, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, device.DeviceCode)
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
	for name, raw := range map[string]string{"email browser": separate.Token, "GitHub browser": github, "CLI": cli} {
		if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s survived the email change: %v", name, err)
		}
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
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, current, p.IdentityID, allMethods); err != nil {
		t.Fatal("email sign-in should permit provider removal", err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, current, current.IdentityID, allMethods); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("email sign-in removed", err)
	}
}

// allMethods offers every sign-in method these tests create.
var allMethods = []string{"passkey", "email", "github", "fixture"}

func TestRemovingASignInMethodLeavesSessionsSignedIn(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	other := f.session(t, user, uuid.Nil())
	sibling, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, other, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	// Removing this browser's own method keeps it signed in with its record.
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, p.IdentityID, allMethods); err != nil {
		t.Fatal(err)
	}
	current, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil || current.SessionID != p.SessionID || current.IdentityID != p.IdentityID {
		t.Fatal("current session changed", current, err)
	}
	// Removing another browser's method keeps that browser signed in too.
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, current, sibling.IdentityID, allMethods); err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, other, uuid.Nil()); err != nil {
		t.Fatal("removing a method signed out another browser", err)
	}
	// Removing a sign-in method still requires recent proof.
	f.session(t, user, uuid.Nil())
	f.exec(t, `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, p.SessionID)
	stale, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	var remaining uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT id FROM auth_identities WHERE user_id=$1 AND revoked_at IS NULL LIMIT 1`, user).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, stale, remaining, allMethods); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatal("stale removal", err)
	}
}

func TestTheLastUsableSignInMethodCannotBeRemoved(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "")
	f.exec(t, `UPDATE users SET primary_email=NULL WHERE id=$1`, user)
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	other, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	// A method the deployment does not offer cannot be signed in with.
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, other.IdentityID, []string{"passkey", "fixture-other"}); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatal("removed a method while no offered one remained", err)
	}
	// Removing a method that is not this browser's is checked the same way.
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, other.IdentityID, allMethods); err != nil {
		t.Fatal(err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, p.IdentityID, allMethods); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatal("removed the last method", err)
	}
	// Email codes to a login address count once email delivery is offered.
	f.exec(t, `UPDATE users SET primary_email='owner@example.test' WHERE id=$1`, user)
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, p.IdentityID, []string{"passkey"}); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatal("email counted without email delivery", err)
	}
	if err = RemoveCredential(t.Context(), f.pool, f.cfg, p, p.IdentityID, []string{"passkey", "email"}); err != nil {
		t.Fatal("email sign-in should keep the account usable", err)
	}
}

func TestSignOutOtherSessionsKeepsOnlyThisBrowser(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	org := f.organization(t, "revoke-others")
	member := f.member(t, org, user, "member")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	other := f.session(t, user, uuid.Nil())
	approver, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, other, org)
	if err != nil {
		t.Fatal(err)
	}
	login, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, approver, login.UserCode); err != nil {
		t.Fatal(err)
	}
	cli, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, login.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, approver, pending.UserCode); err != nil {
		t.Fatal(err)
	}
	// A live Slack link and a pending link request from the other browser.
	registration, installation, link, request := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.exec(t, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,created_by_user_id) VALUES('http',$1,$2,'app',$3)`, registration, org, user)
	f.exec(t, `INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($1,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),'{}',$4)`, installation, registration, org, user)
	f.exec(t, `INSERT INTO slack_user_links(id,installation_id,org_id,member_id,user_id,team_id,slack_user_id) VALUES($1,$2,$3,$4,$5,'team','slack-user')`, link, installation, org, member, user)
	f.exec(t, `INSERT INTO slack_link_requests(id,installation_id,org_id,team_id,slack_user_id,browser_hash,browser_session_id,user_id,member_id,expires_at) VALUES($1,$2,$3,'team','other-slack-user',decode(repeat('33',32),'hex'),$4,$5,$6,clock_timestamp()+interval '10 minutes')`, request, installation, org, approver.SessionID, user, member)
	// Open challenges in both browsers.
	binding := bytes.Repeat([]byte{8}, 32)
	wa, err := webauthn.New(&webauthn.Config{RPDisplayName: "Helmr", RPID: "localhost", RPOrigins: []string{"http://localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	otherEmail, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "owner@example.test", BrowserHash: binding, Session: approver})
	if err != nil {
		t.Fatal(err)
	}
	ownEmail, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "owner@example.test", BrowserHash: binding, Session: p})
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := BeginPasskeyLogin(t.Context(), f.pool, wa, false, binding, approver)
	if err != nil {
		t.Fatal(err)
	}
	ownKey, err := BeginPasskeyLogin(t.Context(), f.pool, wa, false, binding, p)
	if err != nil {
		t.Fatal(err)
	}
	// A pending account repair for this account.
	operatorID, _, operatorToken := f.operator(t)
	operator, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, operatorToken)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := IssueAccountRepair(t.Context(), f.pool, user, operatorID)
	if err != nil {
		t.Fatal(err)
	}
	repairBinding := bytes.Repeat([]byte{6}, 32)
	if _, err = RedeemAccountRepair(t.Context(), f.pool, operator, repair.Capability, repairBinding); err != nil {
		t.Fatal(err)
	}
	repairCeremony, err := BeginAccountRepairRegistration(t.Context(), f.pool, wa, operator, repair.ID, repairBinding)
	if err != nil {
		t.Fatal(err)
	}
	// Stale proof is enough to sign out other sessions.
	f.exec(t, `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, p.SessionID)
	if p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if err = RevokeOtherAccess(t.Context(), f.pool, f.cfg, p); err != nil {
		t.Fatal(err)
	}
	current, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil || current.SessionID != p.SessionID || current.UserID != p.UserID {
		t.Fatal("sign out other sessions ended this browser", current, err)
	}
	for name, token := range map[string]string{"other browser": other, "CLI": cli} {
		if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, token, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s survived: %v", name, err)
		}
	}
	if _, err = ExchangeDeviceCode(t.Context(), f.pool, f.cfg, pending.DeviceCode); err == nil {
		t.Fatal("approved CLI login survived")
	}
	revoked := func(table string, id uuid.UUID) bool {
		t.Helper()
		var gone bool
		if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM `+table+` WHERE id=$1`, id).Scan(&gone); err != nil {
			t.Fatal(table, err)
		}
		return gone
	}
	for name, check := range map[string]struct {
		table string
		id    uuid.UUID
		want  bool
	}{
		"Slack link":                  {"slack_user_links", link, true},
		"Slack link request":          {"slack_link_requests", request, true},
		"other browser email code":    {"email_challenges", otherEmail.ID, true},
		"other browser passkey":       {"webauthn_challenges", otherKey.ChallengeID, true},
		"this browser email code":     {"email_challenges", ownEmail.ID, false},
		"this browser passkey":        {"webauthn_challenges", ownKey.ChallengeID, false},
		"account repair":              {"account_repair_enrollments", repair.ID, true},
		"account repair registration": {"webauthn_challenges", repairCeremony.ChallengeID, true},
	} {
		if got := revoked(check.table, check.id); got != check.want {
			t.Fatalf("%s revoked=%v, want %v", name, got, check.want)
		}
	}
	// The old repair capability cannot restart repair.
	if _, err = BeginAccountRepairRegistration(t.Context(), f.pool, wa, operator, repair.ID, repairBinding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("account repair survived sign out of other sessions", err)
	}
	var facts, notices int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='identity.other_access_revoked' AND target_id=$1`, user).Scan(&facts); err != nil || facts != 1 {
		t.Fatal("missing audit fact", facts, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM account_security_notifications WHERE event='identity.other_access_revoked' AND recipient='owner@example.test'`).Scan(&notices); err != nil || notices != 1 {
		t.Fatal("security notice", notices, err)
	}
	// Signing out everywhere also needs no recent proof and ends this browser.
	if err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, current); err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("sign out everywhere kept this browser", err)
	}
}

func TestDisplayNameChangeDoesNotNeedRecentProof(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, p.SessionID)
	stale, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	if err = UpdateAccountProfile(t.Context(), f.pool, f.cfg, stale, "Renamed"); err != nil {
		t.Fatal("display name change required recent proof", err)
	}
	var name string
	if err = f.pool.QueryRow(t.Context(), `SELECT display_name FROM users WHERE id=$1`, user).Scan(&name); err != nil || name != "Renamed" {
		t.Fatal(name, err)
	}
	if err = RevokeLoginSession(t.Context(), f.pool, f.cfg, raw); err != nil {
		t.Fatal(err)
	}
	if err = UpdateAccountProfile(t.Context(), f.pool, f.cfg, stale, "Signed out"); err == nil {
		t.Fatal("signed-out session changed the display name")
	}
}
