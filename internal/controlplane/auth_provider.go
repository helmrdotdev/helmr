package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/identity"
)

// AuthProvider signs users in through an external identity provider.
type AuthProvider interface {
	RedirectURL(state string, verifier string) string
	Resolve(ctx context.Context, code string, verifier string) (identity.ExternalIdentity, error)
}
