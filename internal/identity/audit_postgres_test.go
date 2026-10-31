package identity

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"io"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestAuditAuthenticationProvenanceAndAtomicCredentials(t *testing.T) {
	f := newIdentityFixture(t)
	external := ExternalIdentity{Provider: "github", Subject: "subject-42", DisplayName: "Original", Email: "person@example.test", EmailVerified: true}
	raw, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", false)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	var issued bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(actor_identity->>'display_name'='Original' AND credential_kind='provider_assertion' AND identity_provider='github' AND details->>'provider'='github' AND details->>'subject'='subject-42') FROM audit_events WHERE action='identity.sign_in' AND target_id=$1`, principal.SessionID).Scan(&issued); err != nil || !issued {
		t.Fatal(issued, err)
	}
	external.DisplayName = "Renamed"
	if _, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT actor_identity->>'display_name'='Original' FROM audit_events WHERE action='identity.sign_in' AND target_id=$1`, principal.SessionID).Scan(&issued); err != nil || !issued {
		t.Fatal("historical identity changed", issued, err)
	}
	linked := external
	linked.Provider = "github"
	linked.Subject = "linked-subject"
	raw, err = LinkProvider(t.Context(), f.pool, f.cfg, principal, linked, raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, linked, "", false)
	if err != nil {
		t.Fatal(err)
	}
	secondPrincipal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, second, uuid.Nil())
	if err != nil || secondPrincipal.UserID != principal.UserID {
		t.Fatal("linked provider identity", secondPrincipal.UserID, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(DISTINCT details->>'subject')=2 FROM audit_events WHERE action='identity.sign_in' AND target_id=ANY($1::uuid[])`, []uuid.UUID{principal.SessionID, secondPrincipal.SessionID}).Scan(&issued); err != nil || !issued {
		t.Fatal("concurrent provider provenance", issued, err)
	}
	f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_issuance CHECK(action<>'identity.sign_in') NOT VALID`)
	if token, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "rejected", DisplayName: "Must rollback", Email: "rollback@example.test", EmailVerified: true}, "", false); err == nil || token != "" {
		t.Fatal("released unaudited credential", token != "", err)
	}
	var absent bool
	if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM auth_identities WHERE subject='rejected')`).Scan(&absent); err != nil || !absent {
		t.Fatal("failed issuance committed identity", absent, err)
	}
	f.exec(t, `ALTER TABLE audit_events DROP CONSTRAINT reject_issuance`)
	f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_revocation CHECK(action<>'identity.session_revoked')`)
	if err := RevokeLoginSession(t.Context(), f.pool, f.cfg, raw); err == nil {
		t.Fatal("unaudited logout succeeded")
	}
	if _, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); err != nil {
		t.Fatal("failed logout changed credential", err)
	}
	f.exec(t, `ALTER TABLE audit_events DROP CONSTRAINT reject_revocation`)
	if err := RevokeLoginSession(t.Context(), f.pool, f.cfg, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	var revoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(credential_id=$1 AND actor_user_id=$2 AND details='{"reason":"logout"}'::jsonb) FROM audit_events WHERE action='identity.session_revoked' AND target_id=$1`, principal.SessionID, principal.UserID).Scan(&revoked); err != nil || !revoked {
		t.Fatal(revoked, err)
	}
}

func TestAuditAuthenticationRefusalSurvivesCancellation(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := t.Context()
	if _, err := CompleteEmailChallenge(ctx, f.pool, f.cfg, uuid.Nil(), nil, "", "", "login"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CompleteEmailChallenge(cancelled, f.pool, f.cfg, uuid.Nil(), nil, "", "", "login"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	var safe bool
	if err := f.pool.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(org_id IS NULL AND authentication_result='failed' AND outcome='denied' AND details='{"reason":"authentication_invalid"}'::jsonb) FROM audit_events WHERE action='identity.sign_in'`).Scan(&safe); err != nil || !safe {
		t.Fatal(safe, err)
	}
	f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_refusal CHECK(outcome<>'denied') NOT VALID`)
	if _, err := CompleteEmailChallenge(ctx, f.pool, f.cfg, uuid.NewV7(), nil, "", "", "login"); !errors.Is(err, audit.ErrCapture) {
		t.Fatal("capture failure hidden", err)
	}
}

// The server may commit issuance before its acknowledgement is lost.
type lostCommitAcknowledgement struct{ db.TxBeginner }

func (b lostCommitAcknowledgement) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return lostCommitTransaction{tx}, nil
}

type lostCommitTransaction struct{ pgx.Tx }

func (tx lostCommitTransaction) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return io.EOF
}
func TestAuditAuthenticationLostCommitAcknowledgement(t *testing.T) {
	f := newIdentityFixture(t)
	_, err := SignInBrowserProvider(t.Context(), lostCommitAcknowledgement{f.pool}, f.cfg, ExternalIdentity{Provider: "github", Subject: "lost-ack", DisplayName: "Committed", Email: "committed@example.test", EmailVerified: true}, "", false)
	if !db.CommitOutcomeUnknown(err) {
		t.Fatalf("expected uncertain commit: %v", err)
	}
	var issued, failed int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE action='identity.sign_in' AND outcome='succeeded'),count(*) FILTER (WHERE action='identity.sign_in' AND outcome IN ('failed','denied')) FROM audit_events`).Scan(&issued, &failed); err != nil {
		t.Fatal(err)
	}
	if issued != 1 || failed != 0 {
		t.Fatalf("issued=%d false failures=%d", issued, failed)
	}
}

func TestAuditDeviceIssuanceLinksVerifiedAuthorization(t *testing.T) {
	f := newIdentityFixture(t)
	raw, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "device-authorizer", DisplayName: "Approver", Email: "approver@example.test", EmailVerified: true}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	org := f.organization(t, "device-audit")
	f.member(t, org, p.UserID, "member")
	p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, org)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, p, started.UserCode); err != nil {
		t.Fatal(err)
	}
	issued, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	device, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, issued, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	var linked bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(i.actor_user_id=$1 AND i.details->>'method'='device_authorization' AND NOT (i.details ? 'provider') AND a.credential_id=$2 AND a.details->>'authorizing_credential_id'=$2::uuid::text) FROM audit_events i JOIN audit_events a ON a.target_id=(i.details->>'device_code_id')::uuid AND a.action='identity.device_authorized' AND a.outcome='succeeded' WHERE i.action='identity.sign_in' AND i.target_id=$3`, p.UserID, p.SessionID, device.SessionID).Scan(&linked); err != nil || !linked {
		t.Fatal("device attribution", linked, err)
	}
	var leaked bool
	if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE to_jsonb(audit_events)::text LIKE '%'||$1||'%' OR to_jsonb(audit_events)::text LIKE '%'||$2||'%')`, started.DeviceCode, issued).Scan(&leaked); err != nil || leaked {
		t.Fatal("credential leaked", leaked, err)
	}
}

func TestAuditSignInCustodyCopiesAndRollback(t *testing.T) {
	for _, method := range []string{"provider", "email", "device"} {
		failures := []string{"none", "platform"}
		if method == "device" {
			failures = append(failures, "organization")
		}
		for _, failure := range failures {
			t.Run(method+"/"+failure, func(t *testing.T) {
				f := newIdentityFixture(t)
				external := ExternalIdentity{Provider: "github", Subject: "custody", DisplayName: "User", Email: "custody@example.test", EmailVerified: true}
				initial, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", false)
				if err != nil {
					t.Fatal(err)
				}
				p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, initial, uuid.Nil())
				if err != nil {
					t.Fatal(err)
				}
				org := f.organization(t, "custody")
				f.member(t, org, p.UserID, "member")
				issue := func() (string, error) { return SignInBrowserProvider(t.Context(), f.pool, f.cfg, external, "", false) }
				if method == "email" {
					c, b := emailChallenge(t, f, external.Email)
					if err = MarkEmailDelivered(t.Context(), f.pool, c); err != nil {
						t.Fatal(err)
					}
					issue = func() (string, error) { r, e := completeEmail(t, f, c, b); return r.Token, e }
				}
				if method == "device" {
					p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, initial, org)
					if err != nil {
						t.Fatal(err)
					}
					started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, p, started.UserCode); err != nil {
						t.Fatal(err)
					}
					issue = func() (string, error) { return ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode) }
				}
				if failure == "platform" {
					f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_copy CHECK(action<>'identity.sign_in' OR org_id IS NOT NULL) NOT VALID`)
				}
				if failure == "organization" {
					f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_copy CHECK(action<>'identity.sign_in' OR org_id IS NULL) NOT VALID`)
				}
				raw, err := issue()
				if failure != "none" {
					if !errors.Is(err, audit.ErrCapture) || raw != "" {
						t.Fatal("unaudited issuance", err)
					}
					if f.activeSessions(t, p.UserID) != 1 {
						t.Fatal("failed audit committed credential")
					}
					f.exec(t, `ALTER TABLE audit_events DROP CONSTRAINT reject_copy`)
					raw, err = issue()
				}
				if err != nil {
					t.Fatal(err)
				}
				issued, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, uuid.Nil())
				if err != nil {
					t.Fatal(err)
				}
				expected := 1
				if method == "device" {
					expected = 2
				}
				var count int
				if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='identity.sign_in' AND target_id=$1 AND outcome='succeeded'`, issued.SessionID).Scan(&count); err != nil || count != expected {
					t.Fatal("custody", count, expected, err)
				}
			})
		}
	}
}

func TestAuditDeviceDenialFailureKeepsDecision(t *testing.T) {
	f := newIdentityFixture(t)
	user := f.user(t, "Approver", "")
	org := f.organization(t, "decision-audit")
	f.member(t, org, user, "member")
	approver, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, f.session(t, user, uuid.Nil()), org)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A real owner transaction failure must remain a denial operation, not approval.
	f.exec(t, `CREATE FUNCTION reject_device_decision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected decision failure'; END $$;
 CREATE TRIGGER reject_device_decision BEFORE UPDATE ON device_codes FOR EACH ROW EXECUTE FUNCTION reject_device_decision()`)
	if _, err := DenyDeviceCode(t.Context(), f.pool, f.cfg, approver, started.UserCode); err == nil {
		t.Fatal("injected failure ignored")
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(action='identity.device_denied' AND outcome='failed' AND actor_user_id=$1 AND details='{"reason":"operation_failed"}'::jsonb)
 FROM audit_events WHERE action IN ('identity.device_authorized','identity.device_denied')`, user).Scan(&valid); err != nil || !valid {
		t.Fatalf("decision failure mislabelled: %v %v", valid, err)
	}
	state, err := DeviceCodeStatus(t.Context(), f.queries, f.cfg, started.UserCode)
	if err != nil || state.Status != "pending" {
		t.Fatalf("decision failed to roll back: %+v %v", state, err)
	}
}

func TestPasskeyFailureAuditsIndependentPrincipalKinds(t *testing.T) {
	f := newIdentityFixture(t)
	for _, operator := range []bool{false, true} {
		_, err := FinishPasskeyLogin(t.Context(), f.pool, f.cfg, nil, operator, uuid.NewV7(), nil, []byte(`{}`), "")
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action IN ('identity.sign_in','operator.sign_in') AND outcome='denied' AND credential_kind='passkey' AND actor_user_id IS NULL AND actor_operator_id IS NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
