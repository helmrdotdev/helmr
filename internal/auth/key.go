package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
)

const RootKeySize = 32

const (
	sessionDomain         = "helmr.auth.session.v0"
	invitationDomain      = "helmr.auth.invitation.v0"
	workerHostDomain      = "helmr.auth.worker-host.v0"
	magicLinkDomain       = "helmr.auth.magic-link.v0"
	deviceCodeDomain      = "helmr.auth.device-code.v0"
	browserAuthDomain     = "helmr.auth.browser-auth.v0"
	telemetryCursorDomain = "helmr.auth.telemetry-cursor.v0"
)

type Keys struct {
	Session         []byte
	Invitation      []byte
	WorkerHost      []byte
	MagicLink       []byte
	DeviceCode      []byte
	BrowserAuth     []byte
	TelemetryCursor []byte
}

func NewKeys(root []byte) (Keys, error) {
	if len(root) != RootKeySize {
		return Keys{}, fmt.Errorf("authentication root key must be %d bytes, got %d", RootKeySize, len(root))
	}
	derive := func(domain string) []byte {
		mac := hmac.New(sha256.New, root)
		_, _ = mac.Write([]byte(domain))
		return mac.Sum(nil)
	}
	return Keys{
		Session:         derive(sessionDomain),
		Invitation:      derive(invitationDomain),
		WorkerHost:      derive(workerHostDomain),
		MagicLink:       derive(magicLinkDomain),
		DeviceCode:      derive(deviceCodeDomain),
		BrowserAuth:     derive(browserAuthDomain),
		TelemetryCursor: derive(telemetryCursorDomain),
	}, nil
}

func (k Keys) Valid() bool {
	return len(k.Session) == RootKeySize &&
		len(k.Invitation) == RootKeySize &&
		len(k.WorkerHost) == RootKeySize &&
		len(k.MagicLink) == RootKeySize &&
		len(k.DeviceCode) == RootKeySize &&
		len(k.BrowserAuth) == RootKeySize &&
		len(k.TelemetryCursor) == RootKeySize
}
