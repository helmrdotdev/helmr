// Package identity owns who a caller is: users and their auth identities,
// login sessions, email codes, device codes and API keys. Operations take
// domain inputs and a principal where authorization applies, own their
// transactions and return the errors declared here; callers map them to their
// transport. Sign-in with an invitation joins the organization through org on
// the sign-in transaction.
package identity

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
)

var (
	ErrIdentityConflict         = errors.New("identity belongs to another credential lifetime")
	ErrAccountProofRequired     = errors.New("ordinary account proof is required")
	ErrRecentProofRequired      = auth.ErrRecentProofRequired
	ErrRateLimited              = errors.New("too many authentication attempts")
	ErrInvalidToken             = errors.New("token is invalid or expired")
	ErrWrongAccount             = errors.New("verified email does not match invitation")
	ErrAlreadyMember            = errors.New("identity is already a member of this organization")
	ErrUserNotFound             = errors.New("user not found")
	ErrOperatorNotFound         = errors.New("operator not found")
	ErrOrganizationAccessDenied = errors.New("organization access is denied")
	ErrOrganizationRequired     = errors.New("organization is required")
	ErrInvalidDeviceCode        = errors.New("invalid device code")
	ErrDeviceCodeNotFound       = errors.New("device code not found")
	ErrDeviceApproverChanged    = errors.New("signed-in account or organization differs from the one reviewed")
	ErrAuthorizationPending     = errors.New("device authorization is pending")
	ErrDeviceAccessDenied       = errors.New("device authorization was denied")
	ErrDeviceCodeExpired        = errors.New("device code has expired")
	ErrAPIKeyNotFound           = errors.New("api key not found")
	ErrAPIKeyManagementRequired = errors.New("api key management permission is required")
	ErrLastSignInMethod         = errors.New("add another sign-in method before removing this one")
)

// InputError reports a caller-supplied value that the identity domain
// rejects, such as an API key name or expiry it does not allow.
type InputError struct {
	message string
}

func (e InputError) Error() string {
	return e.message
}

func invalidInput(format string, args ...any) error {
	return InputError{message: fmt.Sprintf(format, args...)}
}

// A device code stays valid for ten minutes and is polled every five seconds.
const (
	deviceCodeTTL      = 10 * time.Minute
	devicePollInterval = 5 * time.Second
)

// Config holds the keys that hash login credentials.
type Config struct {
	auditPolicy   auditpolicy.Policy
	sessionKey    []byte
	emailCodeKey  []byte
	deviceCodeKey []byte
	invitationKey []byte
}

// NewConfig derives the identity configuration from the authentication keys.
func NewConfig(keys auth.Keys, policy auditpolicy.Policy) Config {
	return Config{
		auditPolicy:   policy,
		sessionKey:    keys.Session,
		emailCodeKey:  keys.EmailCode,
		deviceCodeKey: keys.DeviceCode,
		invitationKey: keys.Invitation,
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
