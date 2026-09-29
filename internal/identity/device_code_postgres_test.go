package identity

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestDeviceCodePostgresApprovalIssuesOneSession(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Developer", "")
	orgID := fixture.organization(t, "device")
	fixture.member(t, orgID, userID, db.OrgMemberRoleDeveloper)
	approver := auth.Actor{UserID: userID, OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleDeveloper}
	consent := DeviceConsent{UserID: userID.String(), OrgID: orgID.String()}

	started, err := StartDeviceCode(ctx, fixture.queries, fixture.cfg)
	if err != nil || started.ExpiresIn != fixture.cfg.Lifetimes().DeviceCode || started.PollInterval != fixture.cfg.Lifetimes().DevicePollInterval {
		t.Fatalf("started = %+v, err = %v", started, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, started.DeviceCode); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("pending exchange error = %v", err)
	}
	state, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, " "+started.UserCode[:4]+" "+started.UserCode[5:]+" ")
	if err != nil || state.Status != "pending" {
		t.Fatalf("status = %+v, err = %v", state, err)
	}

	if _, err := ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, auth.Actor{UserID: userID, Kind: auth.ActorKindSession}, DeviceConsent{UserID: userID.String()}, started.UserCode); !errors.Is(err, ErrOrganizationRequired) {
		t.Fatalf("approval without organization error = %v", err)
	}
	for _, changed := range []DeviceConsent{{UserID: userID.String()}, {UserID: uuid.NewV7().String(), OrgID: orgID.String()}, {}} {
		if _, err := ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, approver, changed, started.UserCode); !errors.Is(err, ErrDeviceApproverChanged) {
			t.Fatalf("changed consent %+v error = %v", changed, err)
		}
	}
	if _, err := ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, ""); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("empty user code error = %v", err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, "ZZZZ-ZZZZ"); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("unknown user code error = %v", err)
	}
	state, err = ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, started.UserCode)
	if err != nil || state.Status != "approved" {
		t.Fatalf("approval = %+v, err = %v", state, err)
	}
	if _, err := DenyDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, started.UserCode); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("decided code denial error = %v", err)
	}

	raw, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, " "+started.DeviceCode+" ")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := AuthenticateLoginSession(ctx, fixture.queries, fixture.cfg, raw)
	if err != nil || principal.UserID != userID || principal.OrgID != orgID || principal.Role != auth.RoleDeveloper {
		t.Fatalf("device principal = %+v, err = %v", principal, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, started.DeviceCode); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("consumed exchange error = %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, "unknown"); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("unknown exchange error = %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, ""); !errors.Is(err, ErrInvalidDeviceCode) {
		t.Fatalf("empty exchange error = %v", err)
	}
}

func TestDeviceCodePostgresDenialAndExpiry(t *testing.T) {
	fixture := newIdentityFixture(t)
	ctx := t.Context()
	userID := fixture.user(t, "Viewer", "")
	orgID := fixture.organization(t, "device-denied")
	fixture.member(t, orgID, userID, db.OrgMemberRoleViewer)
	approver := auth.Actor{UserID: userID, OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleViewer}
	consent := DeviceConsent{UserID: userID.String(), OrgID: orgID.String()}

	denied, err := StartDeviceCode(ctx, fixture.queries, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := DenyDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, denied.UserCode); err != nil || state.Status != "denied" {
		t.Fatalf("denial = %+v, err = %v", state, err)
	}
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, denied.DeviceCode); !errors.Is(err, ErrDeviceAccessDenied) {
		t.Fatalf("denied exchange error = %v", err)
	}

	expired, err := StartDeviceCode(ctx, fixture.queries, fixture.cfg)
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
	if _, err := ExchangeDeviceCode(ctx, fixture.queries, fixture.cfg, expired.DeviceCode); !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("expired exchange error = %v", err)
	}
	if _, err := ApproveDeviceCode(ctx, fixture.queries, fixture.cfg, approver, consent, expired.UserCode); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("expired approval error = %v", err)
	}
	if _, err := DeviceCodeStatus(ctx, fixture.queries, fixture.cfg, "ZZZZ-ZZZZ"); !errors.Is(err, ErrDeviceCodeNotFound) {
		t.Fatalf("unknown status error = %v", err)
	}
}
