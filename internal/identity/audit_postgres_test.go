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
	raw, err := SignIn(t.Context(), f.pool, f.cfg, external)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	var issued bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(actor_identity->>'display_name'='Original' AND credential_kind='provider_assertion' AND identity_provider='github' AND details->>'provider'='github' AND details->>'subject'='subject-42') FROM audit_events WHERE action='identity.sign_in' AND target_id=$1`, principal.SessionID).Scan(&issued); err != nil || !issued {
		t.Fatal(issued, err)
	}
	external.DisplayName = "Renamed"
	if _, err := SignIn(t.Context(), f.pool, f.cfg, external); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT actor_identity->>'display_name'='Original' FROM audit_events WHERE action='identity.sign_in' AND target_id=$1`, principal.SessionID).Scan(&issued); err != nil || !issued {
		t.Fatal("historical identity changed", issued, err)
	}
	linked := external
	linked.Provider = "oidc"
	linked.Subject = "linked-subject"
	second, err := SignIn(t.Context(), f.pool, f.cfg, linked)
	if err != nil {
		t.Fatal(err)
	}
	secondPrincipal, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, second)
	if err != nil || secondPrincipal.UserID != principal.UserID {
		t.Fatal("linked provider identity", secondPrincipal.UserID, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(DISTINCT details->>'provider')=2 FROM audit_events WHERE action='identity.sign_in' AND target_id=ANY($1::uuid[])`, []uuid.UUID{principal.SessionID, secondPrincipal.SessionID}).Scan(&issued); err != nil || !issued {
		t.Fatal("concurrent provider provenance", issued, err)
	}
	f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_issuance CHECK(action<>'identity.sign_in') NOT VALID`)
	if token, err := SignIn(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "rejected", DisplayName: "Must rollback"}); err == nil || token != "" {
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
	if _, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw); err != nil {
		t.Fatal("failed logout changed credential", err)
	}
	f.exec(t, `ALTER TABLE audit_events DROP CONSTRAINT reject_revocation`)
	if err := RevokeLoginSession(t.Context(), f.pool, f.cfg, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal(err)
	}
	var revoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(credential_id=$1 AND actor_user_id=$2) FROM audit_events WHERE action='identity.session_revoked' AND target_id=$1`, principal.SessionID, principal.UserID).Scan(&revoked); err != nil || !revoked {
		t.Fatal(revoked, err)
	}
}

func TestAuditAuthenticationRefusalSurvivesCancellation(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := t.Context()
	if _, err := CompleteMagicLink(ctx, f.pool, f.cfg, "invalid-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CompleteMagicLink(cancelled, f.pool, f.cfg, " "); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	var safe bool
	if err := f.pool.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(org_id IS NULL AND authentication_result='failed' AND outcome='denied' AND details='{"reason":"authentication_invalid"}'::jsonb) FROM audit_events WHERE action='identity.sign_in'`).Scan(&safe); err != nil || !safe {
		t.Fatal(safe, err)
	}
	f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_refusal CHECK(outcome<>'denied') NOT VALID`)
	if _, err := CompleteMagicLink(ctx, f.pool, f.cfg, uuid.NewV7().String()); !errors.Is(err, audit.ErrCapture) {
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
	_, err := SignIn(t.Context(), lostCommitAcknowledgement{f.pool}, f.cfg, ExternalIdentity{Provider: "github", Subject: "lost-ack", DisplayName: "Committed"})
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
	raw, err := SignIn(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "device-authorizer", DisplayName: "Approver"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	org := f.organization(t, "device-audit")
	f.member(t, org, p.UserID, "member")
	p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApproveDeviceCode(t.Context(), f.pool, f.cfg, p, DeviceConsent{UserID: p.UserID.String(), OrgID: org.String()}, started.UserCode); err != nil {
		t.Fatal(err)
	}
	issued, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	device, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, issued)
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
	for _, method := range []string{"provider", "magic_link", "device"} {
		for _, failure := range []string{"none", "installation", "organization"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				f := newIdentityFixture(t)
				external := ExternalIdentity{Provider: "github", Subject: "custody", DisplayName: "Audited user", Email: "custody@example.test", EmailVerified: true}
				initial, err := SignIn(t.Context(), f.pool, f.cfg, external)
				if err != nil {
					t.Fatal(err)
				}
				p, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, initial)
				if err != nil {
					t.Fatal(err)
				}
				org := f.organization(t, "custody")
				if method == "device" {
					f.member(t, org, p.UserID, "member")
				}
				p, err = AuthenticateLoginSession(t.Context(), f.pool, f.cfg, initial)
				if err != nil {
					t.Fatal(err)
				}
				issue := func() (string, error) { return SignIn(t.Context(), f.pool, f.cfg, external) }
				recipient := MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: external.Email}
				if method != "device" {
					_, invitationToken := f.invitation(t, org, external.Email, "member")
					resolved, hash, err := ResolveInvitation(t.Context(), f.queries, f.cfg, invitationToken)
					if err != nil {
						t.Fatal(err)
					}
					issue = func() (string, error) { return SignInWithInvitation(t.Context(), f.pool, f.cfg, hash, external) }
					recipient = invitationRecipient(resolved.InviteeEmail, resolved.OrgID, resolved.ID)
				}
				if method == "magic_link" {
					link, created, err := CreateMagicLink(t.Context(), f.pool, f.cfg, recipient, "/")
					if err != nil || !created {
						t.Fatal("create magic link", created, err)
					}
					if err := MarkMagicLinkSent(t.Context(), f.pool, link); err != nil {
						t.Fatal(err)
					}
					issue = func() (string, error) {
						result, err := CompleteMagicLink(t.Context(), f.pool, f.cfg, link.Token)
						return result.Session, err
					}
				}
				if method == "device" {
					started, err := StartDeviceCode(t.Context(), f.pool, f.cfg)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := ApproveDeviceCode(t.Context(), f.pool, f.cfg, p, DeviceConsent{UserID: p.UserID.String(), OrgID: org.String()}, started.UserCode); err != nil {
						t.Fatal(err)
					}
					issue = func() (string, error) { return ExchangeDeviceCode(t.Context(), f.pool, f.cfg, started.DeviceCode) }
				}
				if failure == "installation" {
					f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_copy CHECK(action<>'identity.sign_in' OR org_id IS NOT NULL) NOT VALID`)
				}
				if failure == "organization" {
					f.exec(t, `ALTER TABLE audit_events ADD CONSTRAINT reject_copy CHECK(action<>'identity.sign_in' OR org_id IS NULL) NOT VALID`)
				}
				raw, err := issue()
				if failure != "none" {
					if !errors.Is(err, audit.ErrCapture) || raw != "" {
						t.Fatal("released unaudited login", raw != "", err)
					}
					var valid bool
					if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM login_sessions WHERE user_id=$1)=1 AND (SELECT count(*) FROM audit_events WHERE action='identity.sign_in' AND actor_user_id=$1)=1`, p.UserID).Scan(&valid); err != nil || !valid {
						t.Fatal("one failed copy committed another copy/credential", valid, err)
					}
					f.exec(t, `ALTER TABLE audit_events DROP CONSTRAINT reject_copy`)
					raw, err = issue()
				}
				if err != nil {
					t.Fatal(err)
				}
				issued, err := AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw)
				if err != nil {
					t.Fatal(err)
				}
				var valid bool
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(*) FILTER(WHERE org_id IS NULL)=1 AND count(*) FILTER(WHERE org_id=$2)=1 AND bool_and(outcome='succeeded' AND authentication_result='verified') FROM audit_events WHERE action='identity.sign_in' AND target_id=$1`, issued.SessionID, org).Scan(&valid); err != nil || !valid {
					t.Fatal("login custody duplication", valid, err)
				}
			})
		}
	}
}
