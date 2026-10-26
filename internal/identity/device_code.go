package identity

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// DeviceAuthorization is a started device authorization: the device code the
// device polls with, the user code a signed-in user approves, how long both
// stay valid and how often the device may poll.
type DeviceAuthorization struct {
	DeviceCode   string
	UserCode     string
	ExpiresIn    time.Duration
	PollInterval time.Duration
}

// DeviceCodeState is the state of a device code as its approver sees it.
// Status is pending, approved, denied, consumed or expired.
type DeviceCodeState struct {
	Status    string
	ExpiresAt time.Time
}

// DeviceConsent is the account and organization a user reviewed before
// deciding a device code, as canonical IDs.
type DeviceConsent struct {
	UserID string
	OrgID  string
}

// StartDeviceCode starts a device authorization.
func StartDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config) (DeviceAuthorization, error) {
	codes, err := auth.GenerateDeviceCodes()
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("generate device code: %w", err)
	}
	deviceHash, err := auth.HashToken(cfg.deviceCodeKey, codes.DeviceCode)
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("hash device code: %w", err)
	}
	userHash, err := auth.HashToken(cfg.deviceCodeKey, auth.NormalizeUserCode(codes.UserCode))
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("hash user code: %w", err)
	}
	ttl := cfg.lifetimes.DeviceCode
	pollInterval := cfg.lifetimes.DevicePollInterval
	id := uuid.NewV7()
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.CreateDeviceCode(ctx, db.CreateDeviceCodeParams{
			ID:                  pgvalue.UUID(id),
			UserCodeHash:        userHash,
			DeviceCodeHash:      deviceHash,
			ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(ttl)),
			PollIntervalSeconds: int32(pollInterval.Seconds()),
		}); err != nil {
			return fmt.Errorf("create device code: %w", err)
		}
		actor := audit.Unauthenticated("absent", "")
		actor.Authorization = audit.NotEvaluated()
		return audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: actor, Action: "identity.device_started", TargetKind: "device_code", TargetID: id, Details: audit.AuthenticationDetails{Method: "device_authorization"}})
	})
	if err != nil {
		return DeviceAuthorization{}, err
	}
	return DeviceAuthorization{DeviceCode: codes.DeviceCode, UserCode: codes.UserCode, ExpiresIn: ttl, PollInterval: pollInterval}, nil
}

// DeviceCodeStatus returns the state of the device code with the user code.
func DeviceCodeStatus(ctx context.Context, q db.Querier, cfg Config, userCode string) (DeviceCodeState, error) {
	hash, err := userCodeHash(cfg, userCode)
	if err != nil {
		return DeviceCodeState{}, err
	}
	device, err := q.GetDeviceCodeByUserCodeHash(ctx, hash)
	if isNoRows(err) {
		return DeviceCodeState{}, ErrDeviceCodeNotFound
	}
	if err != nil {
		return DeviceCodeState{}, fmt.Errorf("load device code: %w", err)
	}
	return DeviceCodeState{Status: deviceStatus(device), ExpiresAt: pgvalue.Time(device.ExpiresAt)}, nil
}

// ApproveDeviceCode approves the pending device code with the user code for
// the approver's organization, so that the device can exchange it for a login
// session of the approver. DenyDeviceCode denies it instead.
func ApproveDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, consent DeviceConsent, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, database, cfg, approver, consent, userCode, true)
}

// DenyDeviceCode denies the pending device code with the user code.
func DenyDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, consent DeviceConsent, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, database, cfg, approver, consent, userCode, false)
}

// decideDeviceCode requires an approver with an organization whose account
// and organization are the ones consented to.
func decideDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, consent DeviceConsent, userCode string, approve bool) (result DeviceCodeState, resultErr error) {
	actor := audit.FromPrincipal(approver)
	actor.Authorization = audit.NotEvaluated()
	defer recordAuthenticationFailure(ctx, database, cfg, audit.Event{Evidence: actor, OrgID: approver.OrgID, Action: "identity.device_authorized", TargetKind: "device_code"}, &resultErr)
	hash, err := userCodeHash(cfg, userCode)
	if err != nil {
		return DeviceCodeState{}, err
	}
	if approver.MemberID == uuid.Nil() {
		return DeviceCodeState{}, ErrOrganizationRequired
	}
	if consent.UserID != approver.UserID.String() || consent.OrgID != approver.OrgID.String() {
		return DeviceCodeState{}, ErrDeviceApproverChanged
	}
	var device db.DeviceCode
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		current, err := access.Current(ctx, tx, approver, auth.Scope{OrgID: approver.OrgID})
		if err != nil {
			return err
		}
		approver = current
		q := db.New(tx)
		if approve {
			device, err = q.ApproveDeviceCode(ctx, db.ApproveDeviceCodeParams{
				OrgID:        pgvalue.UUID(approver.OrgID),
				MemberID:     pgvalue.UUID(approver.MemberID),
				UserCodeHash: hash,
			})
		} else {
			device, err = q.DenyDeviceCode(ctx, db.DenyDeviceCodeParams{
				OrgID:        pgvalue.UUID(approver.OrgID),
				MemberID:     pgvalue.UUID(approver.MemberID),
				UserCodeHash: hash,
			})
		}
		if isNoRows(err) {
			return ErrDeviceCodeNotFound
		}
		if err != nil {
			return fmt.Errorf("resolve device code: %w", err)
		}
		actor := audit.FromPrincipal(approver)
		actor.Authorization = audit.NotEvaluated()
		action := "identity.device_authorized"
		if !approve {
			action = "identity.device_denied"
		}
		return audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: actor, OrgID: approver.OrgID, Action: action, TargetKind: "device_code", TargetID: pgvalue.MustUUIDValue(device.ID), Details: audit.AuthenticationDetails{Method: "device_authorization", AuthorizingCredentialID: approver.SessionID}})
	})
	if err != nil {
		return DeviceCodeState{}, err
	}
	return DeviceCodeState{Status: deviceStatus(device), ExpiresAt: pgvalue.Time(device.ExpiresAt)}, nil
}

// ExchangeDeviceCode consumes an approved device code once and issues a login
// session of its approver for the approved organization in one transaction,
// returning the raw session token. A device code still awaiting a decision is
// ErrAuthorizationPending, a denied one ErrDeviceAccessDenied and an expired
// one ErrDeviceCodeExpired; an unknown or already consumed one is
// ErrInvalidDeviceCode.
func ExchangeDeviceCode(ctx context.Context, txb db.TxBeginner, cfg Config, deviceCode string) (result string, resultErr error) {
	defer recordAuthenticationFailure(ctx, txb, cfg, audit.Event{Evidence: audit.Unauthenticated("failed", "device_code"), Action: "identity.sign_in", TargetKind: "login_session"}, &resultErr)
	hash, err := auth.HashToken(cfg.deviceCodeKey, strings.TrimSpace(deviceCode))
	if err != nil {
		return "", ErrInvalidDeviceCode
	}
	var rawSession string
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		device, err := q.GetDeviceCodeForPoll(ctx, hash)
		if isNoRows(err) {
			return ErrInvalidDeviceCode
		}
		if err != nil {
			return fmt.Errorf("poll device code: %w", err)
		}
		switch deviceStatus(device) {
		case "pending":
			return ErrAuthorizationPending
		case "denied":
			return ErrDeviceAccessDenied
		case "expired":
			return ErrDeviceCodeExpired
		case "approved":
		default:
			return ErrInvalidDeviceCode
		}
		consumed, err := q.ConsumeDeviceCode(ctx, hash)
		if isNoRows(err) {
			return ErrInvalidDeviceCode
		}
		if err != nil {
			return fmt.Errorf("consume device code: %w", err)
		}
		member, err := q.GetOrgMemberForManagement(ctx, db.GetOrgMemberForManagementParams{OrgID: consumed.OrgID, ID: consumed.DecidedByMemberID})
		if err != nil || member.RemovedAt.Valid {
			return ErrInvalidDeviceCode
		}
		rawSession, err = issueLoginSession(ctx, tx, cfg, signedInUser{id: member.UserID, actor: audit.VerifiedDevice(pgvalue.MustUUIDValue(member.UserID), pgvalue.MustUUIDValue(consumed.ID)), authentication: audit.AuthenticationDetails{Method: "device_authorization", DeviceCodeID: pgvalue.MustUUIDValue(consumed.ID)}}, consumed.OrgID)
		return err
	})
	if err != nil {
		return "", err
	}
	return rawSession, nil
}

func userCodeHash(cfg Config, userCode string) ([]byte, error) {
	hash, err := auth.HashToken(cfg.deviceCodeKey, auth.NormalizeUserCode(userCode))
	if err != nil {
		return nil, ErrInvalidDeviceCode
	}
	return hash, nil
}

// deviceStatus reports a pending device code past its expiry as expired.
func deviceStatus(device db.DeviceCode) string {
	if device.Status == db.DeviceCodeStatusPending && time.Now().After(pgvalue.Time(device.ExpiresAt)) {
		return "expired"
	}
	return string(device.Status)
}
