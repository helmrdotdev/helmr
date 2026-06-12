package control

import (
	"context"
	"encoding/json"
)

type authIdentity struct {
	Provider        string
	Subject         string
	DisplayName     string
	ProfileImageURL string
	Email           string
	EmailVerified   bool
	VerifiedEmails  []string
	EmailLookupErr  string
	Claims          json.RawMessage
}

type authProvider interface {
	RedirectURL(state string, verifier string) string
	Resolve(ctx context.Context, code string, verifier string) (authIdentity, error)
}
