package identity

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
func StartDeviceCode(ctx context.Context, q db.Querier, cfg Config) (DeviceAuthorization, error) {
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
	if _, err := q.CreateDeviceCode(ctx, db.CreateDeviceCodeParams{
		ID:                  pgvalue.UUID(uuid.NewV7()),
		UserCodeHash:        userHash,
		DeviceCodeHash:      deviceHash,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(ttl)),
		PollIntervalSeconds: int32(pollInterval.Seconds()),
	}); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("create device code: %w", err)
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
func ApproveDeviceCode(ctx context.Context, q db.Querier, cfg Config, approver auth.Actor, consent DeviceConsent, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, q, cfg, approver, consent, userCode, true)
}

// DenyDeviceCode denies the pending device code with the user code.
func DenyDeviceCode(ctx context.Context, q db.Querier, cfg Config, approver auth.Actor, consent DeviceConsent, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, q, cfg, approver, consent, userCode, false)
}

// decideDeviceCode requires an approver with an organization whose account
// and organization are the ones consented to.
func decideDeviceCode(ctx context.Context, q db.Querier, cfg Config, approver auth.Actor, consent DeviceConsent, userCode string, approve bool) (DeviceCodeState, error) {
	hash, err := userCodeHash(cfg, userCode)
	if err != nil {
		return DeviceCodeState{}, err
	}
	if approver.Role == "" {
		return DeviceCodeState{}, ErrOrganizationRequired
	}
	if consent.UserID != approver.UserID.String() || consent.OrgID != approver.OrgID.String() {
		return DeviceCodeState{}, ErrDeviceApproverChanged
	}
	var device db.DeviceCode
	if approve {
		device, err = q.ApproveDeviceCode(ctx, db.ApproveDeviceCodeParams{
			OrgID:        pgvalue.UUID(approver.OrgID),
			UserID:       pgvalue.UUID(approver.UserID),
			UserCodeHash: hash,
		})
	} else {
		device, err = q.DenyDeviceCode(ctx, db.DenyDeviceCodeParams{
			OrgID:        pgvalue.UUID(approver.OrgID),
			UserID:       pgvalue.UUID(approver.UserID),
			UserCodeHash: hash,
		})
	}
	if isNoRows(err) {
		return DeviceCodeState{}, ErrDeviceCodeNotFound
	}
	if err != nil {
		return DeviceCodeState{}, fmt.Errorf("resolve device code: %w", err)
	}
	return DeviceCodeState{Status: deviceStatus(device), ExpiresAt: pgvalue.Time(device.ExpiresAt)}, nil
}

// ExchangeDeviceCode consumes an approved device code once and issues a login
// session of its approver for the approved organization, returning the raw
// session token. A device code still awaiting a decision is
// ErrAuthorizationPending, a denied one ErrDeviceAccessDenied and an expired
// one ErrDeviceCodeExpired; an unknown or already consumed one is
// ErrInvalidDeviceCode.
func ExchangeDeviceCode(ctx context.Context, q db.Querier, cfg Config, deviceCode string) (string, error) {
	hash, err := auth.HashToken(cfg.deviceCodeKey, strings.TrimSpace(deviceCode))
	if err != nil {
		return "", ErrInvalidDeviceCode
	}
	device, err := q.GetDeviceCodeForPoll(ctx, hash)
	if isNoRows(err) {
		return "", ErrInvalidDeviceCode
	}
	if err != nil {
		return "", fmt.Errorf("poll device code: %w", err)
	}
	switch deviceStatus(device) {
	case "pending":
		return "", ErrAuthorizationPending
	case "denied":
		return "", ErrDeviceAccessDenied
	case "expired":
		return "", ErrDeviceCodeExpired
	case "approved":
	default:
		return "", ErrInvalidDeviceCode
	}
	consumed, err := q.ConsumeDeviceCode(ctx, hash)
	if isNoRows(err) {
		return "", ErrInvalidDeviceCode
	}
	if err != nil {
		return "", fmt.Errorf("consume device code: %w", err)
	}
	return issueLoginSession(ctx, q, cfg, consumed.DecidedByUserID, consumed.OrgID)
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
