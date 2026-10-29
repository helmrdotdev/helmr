package identity

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"time"
	"uuid"
)

func (f identityFixture) operator(t *testing.T) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	op, key := uuid.NewV7(), uuid.NewV7()
	f.exec(t, `INSERT INTO platform_operators(id,label,webauthn_user_handle) VALUES($1,'Operator',uuid_send($1::uuid))`, op)
	f.exec(t, `INSERT INTO passkey_credentials(id,operator_id,credential_id,label,credential_data) VALUES($1,$2,uuid_send($1::uuid),'Security key','{}')`, key, op)
	var raw string
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		raw, err = issueOperatorSession(t.Context(), tx, f.cfg, op, key, 0, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return op, key, raw
}
func TestOperatorIsolationExpiryAndAudit(t *testing.T) {
	f := newIdentityFixture(t)
	op, key, raw := f.operator(t)
	p, expiry, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil || p.OperatorID != op || p.AccessEpoch != 0 || p.UserID != uuid.Nil() {
		t.Fatal(p, err)
	}
	if time.Until(expiry) > 12*time.Hour || time.Until(expiry) < 11*time.Hour {
		t.Fatal("operator lifetime", expiry)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("operator admitted as account", err)
	}
	actor, err := audit.PlatformOperator(p)
	if err != nil {
		t.Fatal("initial epoch authority", err)
	}
	appendEvent := func() error {
		return db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, f.cfg.auditPolicy, audit.Event{Evidence: actor, Action: "identity.sign_in", TargetKind: "login_session", TargetID: p.SessionID, Details: audit.AuthenticationDetails{Method: "passkey"}})
		})
	}
	if err = appendEvent(); err != nil {
		t.Fatal(err)
	}
	var captured bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(actor_operator_id=$1 AND actor_user_id IS NULL AND actor_identity->>'source'='operator' AND actor_identity->>'display_name'='Operator') FROM audit_events`, op).Scan(&captured); err != nil || !captured {
		t.Fatal("operator attribution", captured, err)
	}
	for _, identity := range []string{`{}`, `{"source":"operator"}`, `{"display_name":"Operator"}`} {
		_, bad := f.pool.Exec(t.Context(), `UPDATE audit_events SET actor_identity=$1::jsonb WHERE actor_operator_id=$2`, identity, op)
		var pe *pgconn.PgError
		if !errors.As(bad, &pe) || pe.Code != "23514" {
			t.Fatal("incomplete audit identity accepted", identity, bad)
		}
	}
	f.exec(t, `UPDATE passkey_credentials SET revoked_at=clock_timestamp() WHERE id=$1`, key)
	if err = appendEvent(); err == nil {
		t.Fatal("revoked operator committed audited mutation")
	}
	if _, _, err = AuthenticateOperator(t.Context(), f.pool, f.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked key", err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 FROM audit_events WHERE actor_operator_id=$1`, op).Scan(&captured); err != nil || !captured {
		t.Fatal("history changed", captured, err)
	}
}
func TestOperatorEnrollmentCapabilityOnceAndBounded(t *testing.T) {
	f := newIdentityFixture(t)
	issued, err := IssueOperatorEnrollment(t.Context(), f.pool, "Bootstrap", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(issued.ExpiresAt) > 15*time.Minute || len(issued.Capability) < 40 {
		t.Fatal("enrollment lifetime or capability")
	}
	binding := make([]byte, 32)
	redeemed, err := RedeemOperatorEnrollment(t.Context(), f.pool, issued.Capability, binding)
	if err != nil || redeemed.ID != issued.ID || redeemed.Capability != "" {
		t.Fatal(redeemed, err)
	}
	if _, err = RedeemOperatorEnrollment(t.Context(), f.pool, issued.Capability, binding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("replayed enrollment", err)
	}
	var bound bool
	if err = f.pool.QueryRow(t.Context(), `SELECT capability_hash IS NULL AND browser_hash=$2 FROM operator_passkey_enrollments WHERE id=$1`, issued.ID, binding).Scan(&bound); err != nil || !bound {
		t.Fatal(bound, err)
	}
}
func TestAuthenticationOwnerShapeConstraints(t *testing.T) {
	f := newIdentityFixture(t)
	op, key, raw := f.operator(t)
	p, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "Account", "")
	for _, mutation := range []string{
		`user_id=$2`, `org_id=$2`, `channel='browser'`, `authenticated_at=issued_at-interval '1 second'`, `authenticated_at=expires_at+interval '1 second'`,
	} {
		_, err = f.pool.Exec(t.Context(), `UPDATE login_sessions SET `+mutation+` WHERE id=$1 AND $2::uuid IS NOT NULL`, p.SessionID, user)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || (pe.Code != "23514" && pe.Code != "23503") {
			t.Fatal(mutation, err)
		}
	}
	_, err = f.pool.Exec(t.Context(), `UPDATE passkey_credentials SET user_id=$2 WHERE id=$1`, key, user)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatal("dual key owner", op, err)
	}
}

// A logout must wait for an admitted mutation's owner/authority locks, or win
// first and make the later mutation fail. It cannot commit between check/write.
func TestOperatorLogoutSerializesWithCredentialMutation(t *testing.T) {
	f := newIdentityFixture(t)
	op, key, raw := f.operator(t)
	p, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, _, err = loadKeyUser(t.Context(), tx, uuid.Nil(), op); err != nil {
		t.Fatal(err)
	}
	if _, err = operatorFromSession(t.Context(), tx, p.SessionID); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	done := make(chan error, 1)
	go func() { done <- RevokeOperatorSession(t.Context(), conn, f.cfg, raw) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, conn.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-done:
			t.Fatal("logout crossed admitted mutation", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("logout did not wait on authority lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = operatorSecurityEvent(t.Context(), tx, f.cfg, p, "operator.credential_removed", key); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = RemoveOperatorKey(t.Context(), f.pool, f.cfg, p, uuid.NewV7()); err == nil {
		t.Fatal("mutation admitted after logout")
	}
}

func TestAccountRevocationInvalidatesPendingRepair(t *testing.T) {
	f := newIdentityFixture(t)
	op, _, raw := f.operator(t)
	operator, _, err := AuthenticateOperator(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	c, b := emailChallenge(t, f, "repair@example.test")
	if err = MarkEmailDelivered(t.Context(), f.pool, c); err != nil {
		t.Fatal(err)
	}
	signed, err := completeEmail(t, f, c, b)
	if err != nil {
		t.Fatal(err)
	}
	user, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, signed.Token, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	issued, err := IssueAccountRepair(t.Context(), f.pool, user.UserID, op)
	if err != nil {
		t.Fatal(err)
	}
	binding := make([]byte, 32)
	redeemed, err := RedeemAccountRepair(t.Context(), f.pool, operator, issued.Capability, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, user); err != nil {
		t.Fatal(err)
	}
	err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		_, _, err := lockAccountRepair(t.Context(), tx, operator, redeemed.ID, binding)
		return err
	})
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatal("old recovery capability survived account revocation", err)
	}
}
