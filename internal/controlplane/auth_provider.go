package controlplane

import "context"

type authIdentity struct {
	Provider        string
	Subject         string
	DisplayName     string
	ProfileImageURL string
	Email           string
	EmailVerified   bool
	VerifiedEmails  []string
	EmailLookupErr  string
}

type AuthProvider interface {
	RedirectURL(state string, verifier string) string
	Resolve(ctx context.Context, code string, verifier string) (authIdentity, error)
}
