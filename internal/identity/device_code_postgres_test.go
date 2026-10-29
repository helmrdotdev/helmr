package identity

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestDeviceCodePostgresApprovalIssuesOneSession(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Member", "")
	orgID := fixture.organization(t, "device")
	fixture.member(t, orgID, userID, "member")
	approver, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, fixture.session(t, userID, uuid.Nil()), orgID)
	if err != nil {
		t.Fatal(err)
	}
	consent := DeviceConsent{OrgID: orgID.String()}

	started, err := StartDeviceCode(ctx, fixture.pool, fixture.cfg)
	if err != nil || started.ExpiresIn != fixture.cfg.Lifetimes().DeviceCode || started.PollInterval != fixture.cfg.Lifetimes().DevicePollInterval {
		t.Fatalf("started = %+v, err = %v", started, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("pending exchange error = %v", err)
	}
	state, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, " "+started.UserCode[:4]+" "+started.UserCode[5:]+" ")
	if err != nil || state.Status != "pending" {
		t.Fatalf("status = %+v, err = %v", state, err)
	}

	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, auth.Principal{UserID: userID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7()}, DeviceConsent{}, started.UserCode); !errors.Is(err, ErrOrganizationRequired) {
		t.Fatalf("approval without organization error = %v", err)
	}
	for _, changed := range []DeviceConsent{{}, {OrgID: uuid.NewV7().String()}} {
		if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, changed, started.UserCode); !errors.Is(err, ErrDeviceApproverChanged) {
			t.Fatalf("changed consent %+v error = %v", changed, err)
		}
	}
	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, ""); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("empty user code error = %v", err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, "ZZZZ-ZZZZ"); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("unknown user code error = %v", err)
	}
	state, err = ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, started.UserCode)
	if err != nil || state.Status != "approved" {
		t.Fatalf("approval = %+v, err = %v", state, err)
	}
	if _, err := DenyDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, started.UserCode); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("decided code denial error = %v", err)
	}

	raw, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, " "+started.DeviceCode+" ")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, raw, uuid.Nil())
	if err != nil || principal.UserID != userID || principal.OrgID != orgID || principal.IsOwner {
		t.Fatalf("device principal = %+v, err = %v", principal, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("consumed exchange error = %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, "unknown"); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("unknown exchange error = %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, ""); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("empty exchange error = %v", err)
	}
}

func TestDeviceCodePostgresDenialAndExpiry(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Peer", "")
	orgID := fixture.organization(t, "device-denied")
	fixture.member(t, orgID, userID, "member")
	approver, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, fixture.session(t, userID, uuid.Nil()), orgID)
	if err != nil {
		t.Fatal(err)
	}
	consent := DeviceConsent{OrgID: orgID.String()}

	denied, err := StartDeviceCode(ctx, fixture.pool, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := DenyDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, denied.UserCode); err != nil || state.Status != "denied" {
		t.Fatalf("denial = %+v, err = %v", state, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, denied.DeviceCode); !errors.Is(err, ErrDeviceAccessDenied) {
		t.Fatalf("denied exchange error = %v", err)
	}

	expired, err := StartDeviceCode(ctx, fixture.pool, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := userCodeHash(fixture.cfg, expired.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	fixture.exec(t, `UPDATE device_codes SET expires_at = now() - interval '1 second' WHERE user_code_hash = $1`, hash)
	if state, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, expired.UserCode); err != nil || state.Status != "expired" {
		t.Fatalf("expired status = %+v, err = %v", state, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, expired.DeviceCode); !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("expired exchange error = %v", err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, consent, expired.UserCode); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("expired approval error = %v", err)
	}
	if _, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, "ZZZZ-ZZZZ"); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("unknown status error = %v", err)
	}
}

func TestDeviceCodePostgresFailedExchangeKeepsApprovedCode(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Member", "")
	orgID := fixture.organization(t, "device-atomic")
	fixture.member(t, orgID, userID, "member")
	approver, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, fixture.session(t, userID, uuid.Nil()), orgID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(ctx, fixture.pool, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, DeviceConsent{OrgID: orgID.String()}, started.UserCode); err != nil {
		t.Fatal(err)
	}

	restore := fixture.rejectLoginSessions(t)
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode); err == nil {
		t.Fatal("exchange succeeded without a login session")
	}
	if state, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, started.UserCode); err != nil || state.Status != "approved" {
		t.Fatalf("status after failed exchange = %+v, err = %v, want approved", state, err)
	}
	if count := fixture.sessionCount(t); count != 1 {
		t.Fatalf("sessions after failed exchange = %d, want one browser session", count)
	}
	restore()

	raw, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode)
	if err != nil {
		t.Fatalf("retried exchange error = %v", err)
	}
	if principal, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, raw, uuid.Nil()); err != nil || principal.UserID != userID || principal.OrgID != orgID {
		t.Fatalf("retried principal = %+v, err = %v", principal, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("second exchange error = %v", err)
	}
}

func TestDeviceCodePostgresConcurrentExchangeIssuesOneSession(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Member", "")
	orgID := fixture.organization(t, "device-concurrent")
	fixture.member(t, orgID, userID, "member")
	approver, err := AuthenticateLoginSession(ctx, fixture.pool, fixture.cfg, fixture.session(t, userID, uuid.Nil()), orgID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartDeviceCode(ctx, fixture.pool, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.pool, fixture.cfg, approver, DeviceConsent{OrgID: orgID.String()}, started.UserCode); err != nil {
		t.Fatal(err)
	}

	const exchanges = 2
	start := make(chan struct{})
	errs := make(chan error, exchanges)
	var wg sync.WaitGroup
	for range exchanges {
		wg.Go(func() {
			<-start
			_, err := ExchangeDeviceCode(ctx, fixture.pool, fixture.cfg, started.DeviceCode)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var succeeded, rejected int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInvalidDeviceCode):
			rejected++
		default:
			t.Fatalf("concurrent exchange error = %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent exchanges succeeded = %d rejected = %d, want 1 and 1", succeeded, rejected)
	}
	if count := fixture.activeSessions(t, userID); count != 2 {
		t.Fatalf("sessions after concurrent exchange = %d, want one browser and one CLI session", count)
	}
}

func TestDeviceConfirmationReplacesOnlyExactLiveCLI(t *testing.T) {
	for _, change := range []string{"success", "logout", "source", "membership", "stale-proof"} {
		t.Run(change, func(t *testing.T) {
			f := newIdentityFixture(t)
			ctx := t.Context()
			user := f.user(t, "CLI user", "")
			org := f.organization(t, "confirm")
			f.member(t, org, user, "member")
			browser, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, f.session(t, user, uuid.Nil()), org)
			if err != nil {
				t.Fatal(err)
			}
			start, err := StartDeviceCode(ctx, f.pool, f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ApproveDeviceCode(ctx, f.pool, f.cfg, browser, DeviceConsent{OrgID: org.String()}, start.UserCode); err != nil {
				t.Fatal(err)
			}
			oldToken, err := ExchangeDeviceCode(ctx, f.pool, f.cfg, start.DeviceCode)
			if err != nil {
				t.Fatal(err)
			}
			old, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, oldToken, uuid.Nil())
			if err != nil {
				t.Fatal(err)
			}
			confirmation, err := StartDeviceConfirmation(ctx, f.pool, f.cfg, old)
			if err != nil {
				t.Fatal(err)
			}
			// Browser approval may use another retained factor, but cannot change scope.
			fresh, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, f.session(t, user, uuid.Nil()), org)
			if err != nil {
				t.Fatal(err)
			}
			if change == "stale-proof" {
				f.exec(t, `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, fresh.SessionID)
				fresh, err = access.SessionPrincipal(ctx, f.pool, fresh.SessionID, org)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = ApproveDeviceCode(ctx, f.pool, f.cfg, fresh, DeviceConsent{OrgID: org.String()}, confirmation.UserCode); !errors.Is(err, ErrRecentProofRequired) {
					t.Fatal(err)
				}
				if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, oldToken, uuid.Nil()); err != nil {
					t.Fatal("failed confirmation revoked old CLI", err)
				}
				return
			}
			if _, err = ApproveDeviceCode(ctx, f.pool, f.cfg, fresh, DeviceConsent{OrgID: org.String()}, confirmation.UserCode); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "logout":
				if err = RevokeLoginSession(ctx, f.pool, f.cfg, oldToken); err != nil {
					t.Fatal(err)
				}
			case "source":
				f.exec(t, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, old.IdentityID)
			case "membership":
				f.exec(t, `UPDATE org_members SET suspended_at=clock_timestamp() WHERE id=$1`, old.MemberID)
			}
			token, err := ExchangeDeviceCode(ctx, f.pool, f.cfg, confirmation.DeviceCode)
			if change != "success" {
				if !errors.Is(err, ErrInvalidDeviceCode) {
					t.Fatal("retired CLI replaced", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = AuthenticateLoginSession(ctx, f.pool, f.cfg, oldToken, uuid.Nil()); err == nil {
				t.Fatal("old CLI still valid")
			}
			renewed, err := AuthenticateLoginSession(ctx, f.pool, f.cfg, token, uuid.Nil())
			if err != nil || renewed.MemberID != old.MemberID || renewed.IdentityID != fresh.IdentityID {
				t.Fatal(renewed, err)
			}
			if _, err = ExchangeDeviceCode(ctx, f.pool, f.cfg, confirmation.DeviceCode); !errors.Is(err, ErrInvalidDeviceCode) {
				t.Fatal("replayed replacement", err)
			}
		})
	}
}
