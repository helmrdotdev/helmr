package control

import (
	"context"
	"encoding/json"

	"golang.org/x/oauth2"
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

type tokenAuthProvider interface {
	ResolveWithToken(ctx context.Context, code string, verifier string) (authIdentity, *oauth2.Token, error)
}
