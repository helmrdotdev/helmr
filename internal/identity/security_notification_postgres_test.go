package identity

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"
)

func TestAccountSecurityAuditAndNotificationAtomicity(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Owner", "owner@example.test")
	raw := f.session(t, user, uuid.Nil())
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	invalid := f.cfg
	invalid.auditPolicy = auditpolicy.Policy{}
	if err = RevokeAccountAccess(t.Context(), f.pool, invalid, p); !errors.Is(err, audit.ErrCapture) {
		t.Fatal("expected audit failure", err)
	}
	if _, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); err != nil {
		t.Fatal("failed audit committed revocation", err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM account_security_notifications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed transaction queued mail", count, err)
	}
	if err = RevokeAccountAccess(t.Context(), f.pool, f.cfg, p); err != nil {
		t.Fatal(err)
	}
	var matched bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(actor_user_id=$1 AND target_user_id=$1 AND outcome='succeeded') FROM audit_events WHERE action='identity.access_revoked'`, user).Scan(&matched); err != nil || !matched {
		t.Fatal("security audit missing", matched, err)
	}
	first, found, err := ClaimSecurityNotification(t.Context(), f.pool)
	if err != nil || !found || first.Recipient != "owner@example.test" || first.Event != "identity.access_revoked" {
		t.Fatal(first, found, err)
	}
	if _, found, err = ClaimSecurityNotification(t.Context(), f.pool); err != nil || found {
		t.Fatal("leased mail claimed twice", found, err)
	}
	stale := first
	stale.ClaimID = uuid.NewV7()
	if err = CompleteSecurityNotification(t.Context(), f.pool, stale, true); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM account_security_notifications WHERE status='delivered'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale claim delivered", count, err)
	}
	if err = CompleteSecurityNotification(t.Context(), f.pool, first, false); err != nil {
		t.Fatal(err)
	}
	if _, found, err = ClaimSecurityNotification(t.Context(), f.pool); err != nil || found {
		t.Fatal("retry backoff missing", found, err)
	}
	f.exec(t, `UPDATE account_security_notifications SET next_attempt_at=clock_timestamp()-interval '1 second'`)
	retry, found, err := ClaimSecurityNotification(t.Context(), f.pool)
	if err != nil || !found || retry.ID != first.ID || retry.ClaimID == first.ClaimID {
		t.Fatal("retry identity", retry, found, err)
	}
	if err = CompleteSecurityNotification(t.Context(), f.pool, retry, true); err != nil {
		t.Fatal(err)
	}
	if _, found, err = ClaimSecurityNotification(t.Context(), f.pool); err != nil || found {
		t.Fatal("delivered mail retried", found, err)
	}
}
