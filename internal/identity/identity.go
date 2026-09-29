// Package identity owns who a caller is: users and their auth identities,
// login sessions, magic links, device codes and API keys. Operations take
// domain inputs and a principal where authorization applies, own their
// transactions and return the errors declared here; callers map them to their
// transport. Sign-in with an invitation joins the organization through org on
// the sign-in transaction.
package identity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidToken             = errors.New("token is invalid or expired")
	ErrWrongAccount             = errors.New("verified email does not match invitation")
	ErrAlreadyMember            = errors.New("identity is already a member of this organization")
	ErrInactiveMember           = errors.New("membership is no longer active")
	ErrUserNotFound             = errors.New("user not found")
	ErrOrganizationRequired     = errors.New("organization is required")
	ErrInvalidDeviceCode        = errors.New("invalid device code")
	ErrDeviceCodeNotFound       = errors.New("device code not found")
	ErrDeviceApproverChanged    = errors.New("signed-in account or organization differs from the one reviewed")
	ErrAuthorizationPending     = errors.New("device authorization is pending")
	ErrDeviceAccessDenied       = errors.New("device authorization was denied")
	ErrDeviceCodeExpired        = errors.New("device code has expired")
	ErrAPIKeyNotFound           = errors.New("api key not found")
	ErrAPIKeyManagementRequired = errors.New("api key management permission is required")
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

const (
	defaultSessionTTL         = 30 * 24 * time.Hour
	defaultMagicLinkTTL       = 15 * time.Minute
	defaultDeviceCodeTTL      = 10 * time.Minute
	defaultDevicePollInterval = 5 * time.Second
)

// Lifetimes are how long issued credentials stay valid. A non-positive value
// takes the default.
type Lifetimes struct {
	Session            time.Duration
	MagicLink          time.Duration
	DeviceCode         time.Duration
	DevicePollInterval time.Duration
}

// Config holds the keys that hash login credentials, their lifetimes and the
// verified email addresses whose users become administrators when first
// created.
type Config struct {
	sessionKey    []byte
	magicLinkKey  []byte
	deviceCodeKey []byte
	invitationKey []byte
	lifetimes     Lifetimes
	adminEmails   map[string]struct{}
}

// NewConfig derives the identity configuration from the authentication keys.
func NewConfig(keys auth.Keys, lifetimes Lifetimes, adminEmails []string) Config {
	if lifetimes.Session <= 0 {
		lifetimes.Session = defaultSessionTTL
	}
	if lifetimes.MagicLink <= 0 {
		lifetimes.MagicLink = defaultMagicLinkTTL
	}
	if lifetimes.DeviceCode <= 0 {
		lifetimes.DeviceCode = defaultDeviceCodeTTL
	}
	if lifetimes.DevicePollInterval <= 0 {
		lifetimes.DevicePollInterval = defaultDevicePollInterval
	}
	admins := make(map[string]struct{}, len(adminEmails))
	for _, address := range adminEmails {
		if address = normalizeEmail(address); address != "" {
			admins[address] = struct{}{}
		}
	}
	return Config{
		sessionKey:    keys.Session,
		magicLinkKey:  keys.MagicLink,
		deviceCodeKey: keys.DeviceCode,
		invitationKey: keys.Invitation,
		lifetimes:     lifetimes,
		adminEmails:   admins,
	}
}

// Lifetimes returns the credential lifetimes with defaults applied.
func (c Config) Lifetimes() Lifetimes {
	return c.lifetimes
}

func (c Config) initialAdmin(email string, verified bool) bool {
	if !verified {
		return false
	}
	_, ok := c.adminEmails[normalizeEmail(email)]
	return ok
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
