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
	"github.com/jackc/pgx/v5/pgtype"
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
	ttl, pollInterval := deviceCodeTTL, devicePollInterval
	id := uuid.NewV7()
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := db.New(tx).CreateDeviceCode(ctx, db.CreateDeviceCodeParams{
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
func ApproveDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, database, cfg, approver, userCode, true)
}

// DenyDeviceCode denies the pending device code with the user code.
func DenyDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, userCode string) (DeviceCodeState, error) {
	return decideDeviceCode(ctx, database, cfg, approver, userCode, false)
}

// decideDeviceCode requires a browser approver with an organization. The
// caller has already checked that the approver is the account, organization
// and membership the consent page showed.
func decideDeviceCode(ctx context.Context, database db.TxBeginner, cfg Config, approver auth.Principal, userCode string, approve bool) (result DeviceCodeState, resultErr error) {
	actor := audit.FromPrincipal(approver)
	actor.Authorization = audit.NotEvaluated()
	action := "identity.device_authorized"
	if !approve {
		action = "identity.device_denied"
	}
	defer recordAuthenticationFailure(ctx, database, cfg, audit.Event{Evidence: actor, OrgID: approver.OrgID, Action: action, TargetKind: "device_code"}, &resultErr)
	hash, err := userCodeHash(cfg, userCode)
	if err != nil {
		return DeviceCodeState{}, err
	}
	if approver.MemberID == uuid.Nil() {
		return DeviceCodeState{}, ErrOrganizationRequired
	}
	var device db.DeviceCode
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if approver.Channel != "browser" {
			return ErrInvalidToken
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, approver.UserID); err != nil {
			return err
		}
		current, err := access.Current(ctx, tx, approver, auth.Scope{OrgID: approver.OrgID})
		if err != nil {
			return err
		}
		approver = current
		q := db.New(tx)
		_, err = q.GetDeviceCodeByUserCodeHash(ctx, hash)
		if isNoRows(err) {
			return ErrDeviceCodeNotFound
		}
		if err != nil {
			return err
		}
		if approve {
			// A CLI login never needs recent proof again, so approving one
			// needs a browser that signed in within the last hour.
			if !access.Recent(approver, time.Now()) {
				return ErrRecentProofRequired
			}
			device, err = q.ApproveDeviceCode(ctx, db.ApproveDeviceCodeParams{
				UserID:           pgvalue.UUID(approver.UserID),
				BrowserSessionID: pgvalue.UUID(approver.SessionID),
				OrgID:            pgvalue.UUID(approver.OrgID),
				MemberID:         pgvalue.UUID(approver.MemberID),
				UserCodeHash:     hash,
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
// returning the raw session token. The approving browser session must still be
// live with the same account and membership; the CLI session records that
// browser session's sign-in method and time. A device code still awaiting a
// decision is ErrAuthorizationPending, a denied one ErrDeviceAccessDenied and
// an expired one ErrDeviceCodeExpired; an unknown or already consumed one is
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
		if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, device.UserID); err != nil {
			return err
		}
		consumed, err := q.ConsumeDeviceCode(ctx, hash)
		if isNoRows(err) {
			return ErrInvalidDeviceCode
		}
		if err != nil {
			return fmt.Errorf("consume device code: %w", err)
		}
		org := pgvalue.MustUUIDValue(consumed.OrgID)
		browser, err := access.SessionPrincipal(ctx, tx, pgvalue.MustUUIDValue(consumed.BrowserSessionID), org)
		if err != nil || browser.Channel != "browser" || browser.UserID != pgvalue.MustUUIDValue(consumed.UserID) || browser.MemberID != pgvalue.MustUUIDValue(consumed.DecidedByMemberID) {
			return ErrInvalidDeviceCode
		}
		current, err := access.CurrentHuman(ctx, tx, browser)
		if err != nil {
			return ErrInvalidDeviceCode
		}
		rawSession, err = issueCLISession(ctx, tx, cfg, current, pgvalue.MustUUIDValue(consumed.ID))
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

func nullableUUID(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil()
	}
	return pgvalue.MustUUIDValue(id)
}

// issueCLISession starts a CLI session that records the approving browser
// session's sign-in method and time.
func issueCLISession(ctx context.Context, tx pgx.Tx, cfg Config, p auth.Principal, device uuid.UUID) (string, error) {
	raw, err := auth.GenerateOpaque(loginSessionTokenBytes)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(cfg.sessionKey, raw)
	if err != nil {
		return "", err
	}
	id := uuid.NewV7()
	tag, err := tx.Exec(ctx, `INSERT INTO login_sessions(id,user_id,channel,org_id,member_id,token_hash,access_epoch,identity_id,passkey_id,authenticated_at,expires_at)
 SELECT $1,user_id,'cli',$3,$4,$5,access_epoch,identity_id,passkey_id,authenticated_at,clock_timestamp()+interval '30 days' FROM login_sessions WHERE id=$2 AND channel='browser'`, id, p.SessionID, p.OrgID, p.MemberID, hash)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", ErrInvalidDeviceCode
	}
	if err = appendSessionLifecycle(ctx, tx, cfg, audit.FromPrincipal(p), pgvalue.UUID(p.OrgID), "identity.sign_in", id, audit.AuthenticationDetails{Method: "device_authorization", DeviceCodeID: device}); err != nil {
		return "", err
	}
	return raw, nil
}

// ExtendCLISession keeps a CLI session in use for thirty days after its latest
// request. It writes at most once an hour per session.
func ExtendCLISession(ctx context.Context, database db.DBTX, session uuid.UUID) error {
	_, err := database.Exec(ctx, `UPDATE login_sessions SET last_human_activity_at=clock_timestamp(),expires_at=clock_timestamp()+interval '30 days'
 WHERE id=$1 AND channel='cli' AND revoked_at IS NULL AND expires_at>clock_timestamp() AND last_human_activity_at<clock_timestamp()-interval '1 hour'`, session)
	return err
}
