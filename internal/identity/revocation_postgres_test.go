package identity

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
)

// Revoking an account's access, by the holder or by account repair, retires
// every channel into the account in one transaction. Principals admitted
// before it are refused at their next check without any account-wide counter.
func TestAccountRevocationRetiresEveryChannel(t *testing.T) {
	for _, revocation := range []string{"revoke-all", "account-repair"} {
		t.Run(revocation, func(t *testing.T) {
			f := newIdentityFixture(t)
			wa := testWebAuthn(t)
			binding := bytes.Repeat([]byte{4}, 32)
			user := f.user(t, "Owner", "owner@example.test")
			org := f.organization(t, "revoke-"+revocation)
			member := f.member(t, org, user, "member")
			raw := f.session(t, user, uuid.Nil())
			browser, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
			if err != nil {
				t.Fatal(err)
			}
			scoped, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, org)
			if err != nil {
				t.Fatal(err)
			}
			login, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, scoped, login.UserCode); err != nil {
				t.Fatal(err)
			}
			cliToken, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, login.DeviceCode)
			if err != nil {
				t.Fatal(err)
			}
			cli, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, cliToken, org)
			if err != nil {
				t.Fatal(err)
			}
			approved, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, scoped, approved.UserCode); err != nil {
				t.Fatal(err)
			}
			registration, installation, link, request := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			f.exec(t, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,created_by_user_id) VALUES('http',$1,$2,'app',$3)`, registration, org, user)
			f.exec(t, `INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($1,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),'{}',$4)`, installation, registration, org, user)
			f.exec(t, `INSERT INTO slack_user_links(id,installation_id,org_id,member_id,user_id,team_id,slack_user_id) VALUES($1,$2,$3,$4,$5,'team','slack-user')`, link, installation, org, member, user)
			f.exec(t, `INSERT INTO slack_link_requests(id,installation_id,org_id,team_id,slack_user_id,browser_hash,browser_session_id,user_id,member_id,expires_at) VALUES($1,$2,$3,'team','other-slack-user',decode(repeat('33',32),'hex'),$4,$5,$6,clock_timestamp()+interval '10 minutes')`, request, installation, org, browser.SessionID, user, member)
			slackUser, err := access.SlackPrincipal(t.Context(), f.pool, link)
			if err != nil {
				t.Fatal(err)
			}
			email, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "owner@example.test", BrowserHash: binding, Session: browser})
			if err != nil {
				t.Fatal(err)
			}
			passkey, err := BeginAccountPasskey(t.Context(), f.pool, wa, browser, binding)
			if err != nil {
				t.Fatal(err)
			}
			operatorID, _, operatorToken := f.operator(t)
			operator, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, operatorToken)
			if err != nil {
				t.Fatal(err)
			}
			repair, err := IssueAccountRepair(t.Context(), f.pool, user, operatorID)
			if err != nil {
				t.Fatal(err)
			}
			repairBinding := bytes.Repeat([]byte{5}, 32)
			if _, err = RedeemAccountRepair(t.Context(), f.pool, operator, repair.Capability, repairBinding); err != nil {
				t.Fatal(err)
			}

			switch revocation {
			case "revoke-all":
				err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, browser)
			case "account-repair":
				var options RegistrationOptions
				options, err = BeginAccountRepairRegistration(t.Context(), f.pool, wa, operator, repair.ID, repairBinding)
				if err != nil {
					t.Fatal(err)
				}
				err = CompleteAccountRepairRegistration(t.Context(), f.pool, f.cfg, wa, operator, repair.ID, options.ChallengeID, repairBinding, newTestPasskey(t).register(t, options), "Repaired key")
			}
			if err != nil {
				t.Fatal(err)
			}

			for name, token := range map[string]string{"browser session": raw, "CLI session": cliToken} {
				if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, token, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatalf("%s survived: %v", name, err)
				}
			}
			if _, err = ExchangeDeviceCode(t.Context(), f.pool, f.cfg, approved.DeviceCode); err == nil {
				t.Fatal("approved device code survived")
			}
			retired := func(table string, id uuid.UUID) bool {
				t.Helper()
				var done bool
				if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL OR consumed_at IS NOT NULL FROM `+table+` WHERE id=$1`, id).Scan(&done); err != nil {
					t.Fatal(table, err)
				}
				return done
			}
			for name, check := range map[string]struct {
				table string
				id    uuid.UUID
			}{
				"Slack link request":     {"slack_link_requests", request},
				"email challenge":        {"email_challenges", email.ID},
				"webauthn challenge":     {"webauthn_challenges", passkey.ChallengeID},
				"pending account repair": {"account_repair_enrollments", repair.ID},
			} {
				if !retired(check.table, check.id) {
					t.Fatalf("%s survived", name)
				}
			}
			var linkRevoked bool
			if err = f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM slack_user_links WHERE id=$1`, link).Scan(&linkRevoked); err != nil || !linkRevoked {
				t.Fatal("Slack link survived", linkRevoked, err)
			}

			// Principals admitted before the revocation are refused at every check.
			if _, err = access.SessionPrincipal(t.Context(), f.pool, browser.SessionID, uuid.Nil()); !errors.Is(err, access.ErrDenied) {
				t.Fatal("SessionPrincipal admitted a revoked session", err)
			}
			for name, p := range map[string]auth.Principal{"browser": scoped, "CLI": cli, "Slack": slackUser} {
				if _, err = access.CurrentHuman(t.Context(), f.pool, p); !errors.Is(err, access.ErrDenied) {
					t.Fatalf("CurrentHuman admitted the %s principal: %v", name, err)
				}
			}
			if _, err = access.SlackPrincipal(t.Context(), f.pool, link); !errors.Is(err, access.ErrDenied) {
				t.Fatal("SlackPrincipal admitted a revoked link", err)
			}
			if err = RecordHumanActivity(t.Context(), f.pool, browser); err == nil {
				t.Fatal("activity refreshed a revoked session")
			}
			if err = requireRecentAccount(t.Context(), f.pool, browser); err == nil {
				t.Fatal("recent proof accepted from a revoked session")
			}
		})
	}
}

// An email change in progress belongs to the session that started it; once
// that session ends, the change cannot continue.
func TestEmailChangeChallengeEndsWithItsSession(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	binding := bytes.Repeat([]byte{3}, 32)
	c, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "new@example.test", BrowserHash: binding, Session: p, EmailChange: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckEmailChangeChallenge(t.Context(), f.pool, p, c.ID, binding); err != nil {
		t.Fatal(err)
	}
	// Ending the session alone, without retiring its challenges, ends the change.
	f.exec(t, `UPDATE login_sessions SET expires_at=clock_timestamp() WHERE id=$1`, p.SessionID)
	if err = CheckEmailChangeChallenge(t.Context(), f.pool, p, c.ID, binding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("email change continued from an expired session", err)
	}
	f.exec(t, `UPDATE login_sessions SET expires_at=clock_timestamp()+interval '1 hour',revoked_at=clock_timestamp() WHERE id=$1`, p.SessionID)
	if err = CheckEmailChangeChallenge(t.Context(), f.pool, p, c.ID, binding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("email change continued from a revoked session", err)
	}
}
