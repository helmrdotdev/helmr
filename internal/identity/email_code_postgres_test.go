package identity

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"uuid"
)

func emailChallenge(t *testing.T, f identityFixture, email string) (EmailChallenge, []byte) {
	t.Helper()
	binding := bytes.Repeat([]byte{7}, 32)
	c, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: email, BrowserHash: binding, Next: "/settings/account"})
	if err != nil {
		t.Fatal(err)
	}
	return c, binding
}
func completeEmail(t *testing.T, f identityFixture, c EmailChallenge, b []byte) (EmailCompletion, error) {
	t.Helper()
	return CompleteEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, c.Code, "", "login")
}
func TestEmailCodeDeliveryBindingConsumption(t *testing.T) {
	f := newIdentityFixture(t)
	c, b := emailChallenge(t, f, "admin@example.test")
	if _, err := completeEmail(t, f, c, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("undelivered", err)
	}
	if err := MarkEmailDelivery(t.Context(), f.pool, c, true); err != nil {
		t.Fatal(err)
	}
	if _, err := completeEmail(t, f, c, bytes.Repeat([]byte{8}, 32)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("wrong browser", err)
	}
	if _, err := CompleteEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, c.Code, "", "email_change"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("wrong purpose", err)
	}
	result, err := completeEmail(t, f, c, b)
	if err != nil || result.Next != "/settings/account" {
		t.Fatal(result, err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, result.Token, uuid.Nil())
	if err != nil || p.OperatorID != uuid.Nil() || p.OrgID != uuid.Nil() || p.IdentityID == uuid.Nil() {
		t.Fatal(p, err)
	}
	var proof bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(credential_kind='email_code' AND credential_id=$1) FROM audit_events WHERE action='identity.sign_in' AND outcome='succeeded'`, p.IdentityID).Scan(&proof); err != nil || !proof {
		t.Fatal("audit source", proof, err)
	}
	if _, err = completeEmail(t, f, c, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("replay", err)
	}
	if _, _, err = AuthenticateOperator(t.Context(), f.pool, f.cfg, result.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("ordinary token became operator", err)
	}
	expired, b := emailChallenge(t, f, "expired@example.test")
	if err = MarkEmailDelivery(t.Context(), f.pool, expired, true); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE email_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.ID)
	if _, err = completeEmail(t, f, expired, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expiry", err)
	}
}
func TestEmailCodeLimitsAndResend(t *testing.T) {
	f := newIdentityFixture(t)
	c, b := emailChallenge(t, f, "limited@example.test")
	if err := MarkEmailDelivery(t.Context(), f.pool, c, false); err != nil {
		t.Fatal(err)
	}
	if _, err := completeEmail(t, f, c, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("failed delivery", err)
	}
	if _, err := ResendEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, "login"); !errors.Is(err, ErrRateLimited) {
		t.Fatal("cooldown", err)
	}
	f.exec(t, `UPDATE email_challenges SET next_send_at=clock_timestamp()-interval '1 second' WHERE id=$1`, c.ID)
	newer, err := ResendEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, "login")
	if err != nil {
		t.Fatal(err)
	}
	// A delayed success for an obsolete delivery cannot mark the new generation delivered.
	if err = MarkEmailDelivery(t.Context(), f.pool, c, true); err != nil {
		t.Fatal(err)
	}
	if _, err = completeEmail(t, f, newer, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("stale delivery", err)
	}
	if err = MarkEmailDelivery(t.Context(), f.pool, newer, true); err != nil {
		t.Fatal(err)
	}
	if _, err = completeEmail(t, f, c, b); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("superseded code", err)
	}
	for i := 0; i < 4; i++ {
		if _, err = CompleteEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, "invalid", "", "login"); !errors.Is(err, ErrInvalidToken) {
			t.Fatal(i, err)
		}
	}
	if _, err = completeEmail(t, f, newer, b); !errors.Is(err, ErrRateLimited) {
		t.Fatal("guess limit", err)
	}
	for i := 0; i < 4; i++ {
		emailChallenge(t, f, "LIMITED@example.test")
	}
	if _, err = StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "limited@example.test", BrowserHash: b}); !errors.Is(err, ErrRateLimited) {
		t.Fatal("address limit", err)
	}
	emailChallenge(t, f, "other@example.test")
}
func TestEmailCodeInvitationRequiresConsent(t *testing.T) {
	f := newIdentityFixture(t)
	org := f.organization(t, "invitation")
	inv, token := f.invitation(t, org, "joiner@example.test", "member")
	b := bytes.Repeat([]byte{4}, 32)
	c, err := StartEmailChallenge(t.Context(), f.pool, f.cfg, EmailStart{Email: "joiner@example.test", BrowserHash: b, InvitationID: uuid.UUID(inv.ID.Bytes)})
	if err != nil {
		t.Fatal(err)
	}
	if err = MarkEmailDelivery(t.Context(), f.pool, c, true); err != nil {
		t.Fatal(err)
	}
	done, err := CompleteEmailChallenge(t.Context(), f.pool, f.cfg, c.ID, b, c.Code, "", "invite")
	if err != nil {
		t.Fatal(err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, done.Token, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM org_members WHERE org_id=$1 AND user_id=$2`, org, p.UserID).Scan(&count); err != nil || count != 0 {
		t.Fatal("sign-in accepted invitation", count, err)
	}
	accepted, err := AcceptAccountInvitation(t.Context(), f.pool, f.cfg, p, token)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := AcceptAccountInvitation(t.Context(), f.pool, f.cfg, p, token)
	if err != nil || accepted != retry {
		t.Fatal("retry", retry, err)
	}
	f.exec(t, `UPDATE users SET primary_email='changed@example.test' WHERE id=$1`, p.UserID)
	retry, err = AcceptAccountInvitation(t.Context(), f.pool, f.cfg, p, token)
	if err != nil || accepted != retry {
		t.Fatal("retry after email change", retry, err)
	}
	f.exec(t, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, accepted.MembershipID)
	if _, err = AcceptAccountInvitation(t.Context(), f.pool, f.cfg, p, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("retry resurrected member", err)
	}
}
