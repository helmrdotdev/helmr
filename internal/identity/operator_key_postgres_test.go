package identity

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/helmrdotdev/helmr/internal/auth"
)

// installOperatorKey redeems a deployment-issued capability in a new browser and
// registers key with it.
func installOperatorKey(t *testing.T, f identityFixture, wa *webauthn.WebAuthn, issued OperatorEnrollment, key *testPasskey, label string) {
	t.Helper()
	binding := make([]byte, 32)
	copy(binding, issued.ID[:])
	redeemed, err := RedeemOperatorEnrollment(t.Context(), f.pool, issued.Capability, binding)
	if err != nil {
		t.Fatal(err)
	}
	options, err := BeginOperatorRegistration(t.Context(), f.pool, wa, redeemed.ID, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = CompleteOperatorRegistration(t.Context(), f.pool, f.cfg, wa, redeemed.ID, options.ChallengeID, binding, key.register(t, options), label); err != nil {
		t.Fatal(err)
	}
	// The spent enrollment cannot register another key.
	if _, err = BeginOperatorRegistration(t.Context(), f.pool, wa, redeemed.ID, binding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("completed enrollment registered again", err)
	}
}

func signInOperator(t *testing.T, f identityFixture, wa *webauthn.WebAuthn, key *testPasskey, raw string) string {
	t.Helper()
	binding := bytes.Repeat([]byte{2}, 32)
	existing := auth.Principal{}
	if raw != "" {
		p, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
		if err != nil {
			t.Fatal(err)
		}
		existing = p
	}
	options, err := BeginPasskeyLogin(t.Context(), f.pool, wa, true, binding, existing)
	if err != nil {
		t.Fatal(err)
	}
	token, err := FinishPasskeyLogin(t.Context(), f.pool, f.cfg, wa, true, options.ChallengeID, binding, key.assert(t, options), raw)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f identityFixture) operatorAudit(t *testing.T, op uuid.UUID, action string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action=$1 AND target_id=$2`, action, op).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOperatorEnrollmentInstallsTheRegisteredKey(t *testing.T) {
	f := newIdentityFixture(t)
	wa := testWebAuthn(t)
	issued, err := IssueOperatorEnrollment(t.Context(), f.pool, "Bootstrap", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	first := newTestPasskey(t)
	installOperatorKey(t, f, wa, issued, first, "First key")
	var op uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT operator_id FROM operator_passkey_enrollments WHERE id=$1`, issued.ID).Scan(&op); err != nil {
		t.Fatal(err)
	}
	if f.operatorAudit(t, op, "operator.credential_added") != 1 || f.operatorAudit(t, op, "operator.repaired") != 0 {
		t.Fatal("first key was not recorded as added")
	}
	raw := signInOperator(t, f, wa, first, "")

	// Repair keeps the operator's name, so it takes no label.
	if _, err = IssueOperatorEnrollment(t.Context(), f.pool, "Renamed", op); !errors.As(err, &InputError{}) {
		t.Fatal("repair accepted a label", err)
	}
	if _, err = IssueOperatorEnrollment(t.Context(), f.pool, "", uuid.NewV7()); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatal("repair of an unknown operator", err)
	}
	repair, err := IssueOperatorEnrollment(t.Context(), f.pool, "", op)
	if err != nil {
		t.Fatal(err)
	}
	second := newTestPasskey(t)
	installOperatorKey(t, f, wa, repair, second, "Replacement key")
	if f.operatorAudit(t, op, "operator.repaired") != 1 {
		t.Fatal("repair was not recorded")
	}
	if _, _, err = AuthenticateOperator(t.Context(), f.pool, f.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("repair kept an existing operator session", err)
	}
	var live int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM passkey_credentials WHERE operator_id=$1 AND revoked_at IS NULL AND label='Replacement key'`, op).Scan(&live); err != nil || live != 1 {
		t.Fatal("repair did not leave exactly the new key", live, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM passkey_credentials WHERE operator_id=$1 AND revoked_at IS NULL`, op).Scan(&live); err != nil || live != 1 {
		t.Fatal("repair kept an old key", live, err)
	}
	signInOperator(t, f, wa, second, "")
}

func TestOperatorAddsKeyThroughItsOwnSession(t *testing.T) {
	f := newIdentityFixture(t)
	wa := testWebAuthn(t)
	issued, err := IssueOperatorEnrollment(t.Context(), f.pool, "Operator", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	first := newTestPasskey(t)
	installOperatorKey(t, f, wa, issued, first, "First key")
	raw := signInOperator(t, f, wa, first, "")
	p, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	binding := bytes.Repeat([]byte{6}, 32)
	options, err := BeginOperatorKey(t.Context(), f.pool, wa, p, binding)
	if err != nil {
		t.Fatal(err)
	}
	second := newTestPasskey(t)
	response := second.register(t, options)

	// Another live session of the same operator cannot finish this ceremony.
	other, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, signInOperator(t, f, wa, first, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err = FinishOperatorKey(t.Context(), f.pool, f.cfg, wa, other, options.ChallengeID, binding, response, "Second key"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("another session finished the ceremony", err)
	}
	// An account session cannot finish an operator ceremony either.
	user := f.user(t, "Account", "account@example.test")
	account, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	if err = FinishAccountPasskey(t.Context(), f.pool, f.cfg, wa, account, options.ChallengeID, binding, response, "Second key"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("account finished an operator ceremony", err)
	}
	if err = FinishOperatorKey(t.Context(), f.pool, f.cfg, wa, p, options.ChallengeID, binding, response, "Second key"); err != nil {
		t.Fatal(err)
	}
	var live int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM passkey_credentials WHERE operator_id=$1 AND revoked_at IS NULL`, p.OperatorID).Scan(&live); err != nil || live != 2 {
		t.Fatal("added key missing", live, err)
	}
	if f.operatorAudit(t, p.OperatorID, "operator.credential_added") != 2 {
		t.Fatal("added key not recorded")
	}
	signInOperator(t, f, wa, second, "")

	// Both steps need a recent confirmation.
	f.exec(t, `UPDATE login_sessions SET issued_at=clock_timestamp()-interval '2 hours',authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, p.SessionID)
	if _, err = BeginOperatorKey(t.Context(), f.pool, wa, p, binding); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatal("stale session started a key ceremony", err)
	}
	options, err = BeginOperatorKey(t.Context(), f.pool, wa, other, binding)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE login_sessions SET issued_at=clock_timestamp()-interval '2 hours',authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, other.SessionID)
	if err = FinishOperatorKey(t.Context(), f.pool, f.cfg, wa, other, options.ChallengeID, binding, newTestPasskey(t).register(t, options), "Third key"); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatal("stale session finished a key ceremony", err)
	}
}

func TestOperatorConfirmationExtendsTheSameSession(t *testing.T) {
	f := newIdentityFixture(t)
	wa := testWebAuthn(t)
	issued, err := IssueOperatorEnrollment(t.Context(), f.pool, "Operator", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	key := newTestPasskey(t)
	installOperatorKey(t, f, wa, issued, key, "Key")
	raw := signInOperator(t, f, wa, key, "")
	before, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE login_sessions SET issued_at=clock_timestamp()-interval '10 hours',authenticated_at=clock_timestamp()-interval '10 hours',expires_at=clock_timestamp()+interval '2 hours' WHERE id=$1`, before.SessionID)
	confirmed := signInOperator(t, f, wa, key, raw)
	if _, _, err = AuthenticateOperator(t.Context(), f.pool, f.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("confirmation kept the old token", err)
	}
	after, expiry, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionID != before.SessionID || !auth.ProvedRecently(after.AuthenticatedAt, time.Now()) {
		t.Fatal("confirmation did not keep and refresh the session", before, after)
	}
	if left := time.Until(expiry); left < 11*time.Hour || left > 12*time.Hour {
		t.Fatal("confirmation did not extend the session to twelve hours", left)
	}
}

// Repair signs out every operator session, so ceremonies those sessions began
// cannot finish, even with the newly installed key.
func TestOperatorRepairEndsCeremoniesOfOldSessions(t *testing.T) {
	f := newIdentityFixture(t)
	wa := testWebAuthn(t)
	issued, err := IssueOperatorEnrollment(t.Context(), f.pool, "Operator", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	old := newTestPasskey(t)
	installOperatorKey(t, f, wa, issued, old, "Old key")
	raw := signInOperator(t, f, wa, old, "")
	p, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	binding := bytes.Repeat([]byte{2}, 32)
	add, err := BeginOperatorKey(t.Context(), f.pool, wa, p, binding)
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := BeginPasskeyLogin(t.Context(), f.pool, wa, true, binding, p)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := IssueOperatorEnrollment(t.Context(), f.pool, "", p.OperatorID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := newTestPasskey(t)
	installOperatorKey(t, f, wa, repair, replacement, "Replacement key")
	if err = FinishOperatorKey(t.Context(), f.pool, f.cfg, wa, p, add.ChallengeID, binding, newTestPasskey(t).register(t, add), "Added key"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("add-key ceremony survived repair", err)
	}
	if _, err = FinishPasskeyLogin(t.Context(), f.pool, f.cfg, wa, true, confirm.ChallengeID, binding, replacement.assert(t, confirm), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("confirmation survived repair", err)
	}
	if _, _, err = AuthenticateOperator(t.Context(), f.pool, f.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("old operator session survived repair", err)
	}
}
