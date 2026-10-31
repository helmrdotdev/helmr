package identity

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestAccountChangesCancelBoundChallengesAcrossSessions(t *testing.T) {
	for _, operation := range []string{"revoke-all", "email-change"} {
		t.Run(operation, func(t *testing.T) {
			f := newIdentityFixture(t)
			user := f.user(t, "Owner", "owner@example.test")
			other := f.user(t, "Other", "other@example.test")
			binding := bytes.Repeat([]byte{9}, 32)
			wa, err := webauthn.New(&webauthn.Config{RPDisplayName: "Helmr", RPID: "localhost", RPOrigins: []string{"http://localhost"}})
			if err != nil {
				t.Fatal(err)
			}
			var principals []auth.Principal
			var tokens []string
			var emails []EmailChallenge
			var ceremonies []AssertionOptions
			for _, owner := range []uuid.UUID{user, user, other} {
				raw := f.session(t, owner, uuid.Nil())
				p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
				if err != nil {
					t.Fatal(err)
				}
				address := "owner@example.test"
				if owner == other {
					address = "other@example.test"
				}
				c, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: address, BrowserHash: binding, Session: p})
				if err != nil {
					t.Fatal(err)
				}
				assertion, err := BeginPasskeyLogin(t.Context(), f.pool, wa, false, binding, p)
				if err != nil {
					t.Fatal(err)
				}
				principals, tokens = append(principals, p), append(tokens, raw)
				emails, ceremonies = append(emails, c), append(ceremonies, assertion)
			}
			anonymous, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "owner@example.test", BrowserHash: binding})
			if err != nil {
				t.Fatal(err)
			}
			// Revocation must include requests bound to an already-retired session.
			f.exec(t, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, principals[1].SessionID)
			if operation == "revoke-all" {
				err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, principals[0])
			} else {
				var change EmailChallenge
				change, err = StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "new@example.test", BrowserHash: binding, Session: principals[0], EmailChange: true})
				if err != nil {
					t.Fatal(err)
				}
				if err = MarkEmailDelivered(t.Context(), f.pool, change); err != nil {
					t.Fatal(err)
				}
				var completed EmailCompletion
				completed, err = CompleteEmailChallenge(t.Context(), f.pool, f.cfg, change.ID, binding, change.Code, tokens[0], "email_change")
				if err == nil {
					p, e := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, completed.Token, uuid.Nil())
					if e != nil || p.SessionID != principals[0].SessionID {
						t.Fatal("mailbox change lost the current session", p, e)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			open := func(id uuid.UUID) bool {
				t.Helper()
				var live bool
				if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NULL AND consumed_at IS NULL FROM email_challenges WHERE id=$1`, id).Scan(&live); err != nil {
					t.Fatal(err)
				}
				return live
			}
			for _, c := range emails[:2] {
				if open(c.ID) {
					t.Fatal("cancelled challenge still open")
				}
				if _, err = ResendEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, binding, "login"); !errors.Is(err, ErrInvalidToken) {
					t.Fatal("cancelled challenge can resend", err)
				}
			}
			for _, c := range []EmailChallenge{emails[2], anonymous} {
				if !open(c.ID) {
					t.Fatal("unrelated or unbound challenge cancelled")
				}
			}
			if operation == "revoke-all" {
				for i, c := range ceremonies {
					var revoked bool
					if err = f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM webauthn_challenges WHERE id=$1`, c.ChallengeID).Scan(&revoked); err != nil || revoked != (i < 2) {
						t.Fatal("wrong ceremony cancellation set", i, revoked, err)
					}
				}
			}
		})
	}
}

func TestDeviceExchangeCopiesTheBrowserSignInAtExchange(t *testing.T) {
	for _, change := range []string{"same-source", "different-source", "revoke-all"} {
		t.Run(change, func(t *testing.T) {
			f := newIdentityFixture(t)
			external := ExternalIdentity{Provider: "github", Subject: "device-owner", DisplayName: "Owner", Email: "owner@example.test", EmailVerified: true}
			raw, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", uuid.Nil())
			if err != nil {
				t.Fatal(err)
			}
			p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
			if err != nil {
				t.Fatal(err)
			}
			org := f.organization(t, "device-reproof")
			f.member(t, org, p.UserID, "member")
			p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, org)
			if err != nil {
				t.Fatal(err)
			}
			device, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, p, device.UserCode); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "same-source":
				_, err = SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, raw, p.SessionID)
			case "different-source":
				_, err = LinkProvider(t.Context(), f.pool, f.cfg, p, ExternalIdentity{Provider: "github", Subject: "second-source", Email: "owner@example.test", EmailVerified: true}, raw)
			case "revoke-all":
				err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, p)
			}
			if err != nil {
				t.Fatal(err)
			}
			cli, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, device.DeviceCode)
			if change == "revoke-all" {
				if err == nil {
					t.Fatal("revoked browser exchanged an old approval")
				}
				return
			}
			if err != nil {
				t.Fatal("browser sign-in again blocked the approved CLI login", err)
			}
			issued, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, cli, uuid.Nil())
			if err != nil || issued.MemberID != p.MemberID || issued.UserID != p.UserID {
				t.Fatal(issued, err)
			}
			var copied bool
			if err = f.pool.QueryRow(t.Context(), `SELECT c.identity_id IS NOT DISTINCT FROM b.identity_id AND c.passkey_id IS NOT DISTINCT FROM b.passkey_id AND c.authenticated_at=b.authenticated_at
 FROM login_sessions c, login_sessions b WHERE c.id=$1 AND b.id=$2`, issued.SessionID, p.SessionID).Scan(&copied); err != nil || !copied {
				t.Fatal("CLI did not copy the browser's current sign-in", copied, err)
			}
		})
	}
}

func TestAccountRevocationCancelsEnrollmentBoundCeremony(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	operator, _, raw := f.operator(t)
	op, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := IssueAccountRepair(t.Context(), f.pool, user, operator)
	if err != nil {
		t.Fatal(err)
	}
	binding := bytes.Repeat([]byte{5}, 32)
	if _, err = RedeemAccountRepair(t.Context(), f.pool, op, repair.Capability, binding); err != nil {
		t.Fatal(err)
	}
	wa, err := webauthn.New(&webauthn.Config{RPDisplayName: "Helmr", RPID: "localhost", RPOrigins: []string{"http://localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	ceremony, err := BeginAccountRepairRegistration(t.Context(), f.pool, wa, op, repair.ID, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, p); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err = f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM webauthn_challenges WHERE id=$1`, ceremony.ChallengeID).Scan(&revoked); err != nil || !revoked {
		t.Fatal("enrollment-bound ceremony survived", revoked, err)
	}
	if _, err = ReadAccountRepair(t.Context(), f.pool, op, repair.ID, binding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("enrollment adopted the new account epoch", err)
	}
}
