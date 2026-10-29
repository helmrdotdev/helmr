package identity

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/net/publicsuffix"
)

// NewWebAuthn fixes the installation's relying party to its configured public
// origin. Tenant and operator separation is enforced by the credential owner and
// challenge transaction, not by authenticator display names.
func NewWebAuthn(origin, rpID, displayName string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return nil, fmt.Errorf("passkey origin must be an absolute origin without a path, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, fmt.Errorf("passkey origin requires HTTPS (HTTP is allowed only on localhost)")
	}
	if rpID == "" || (u.Hostname() != rpID && !strings.HasSuffix(u.Hostname(), "."+rpID)) || strings.ContainsAny(rpID, "/:@ ") {
		return nil, fmt.Errorf("passkey relying party ID must be the origin host or its parent domain")
	}
	if suffix, _ := publicsuffix.PublicSuffix(rpID); suffix == rpID && rpID != "localhost" {
		return nil, fmt.Errorf("passkey relying party ID must not be a public suffix")
	}
	if strings.TrimSpace(displayName) == "" {
		return nil, fmt.Errorf("passkey relying party display name is required")
	}
	return webauthn.New(&webauthn.Config{
		RPID: rpID, RPDisplayName: displayName, RPOrigins: []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: 10 * time.Minute},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: 10 * time.Minute},
		},
	})
}

// passkeyUser is loaded for one exact account or operator. The handle is random
// and stable for that owner; the account ID and email are never used as handles.
type passkeyUser struct {
	handle      []byte
	name        string
	credentials []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u passkeyUser) WebAuthnName() string                       { return u.name }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.name }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
