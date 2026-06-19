package waitpoint

import (
	"net/url"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	callbackSecretPrefix = "hlmr_wpc_"
	tokenBytes           = 32
)

func NewCallbackSecret(authSecret []byte) (string, []byte, error) {
	raw, err := auth.GenerateOpaqueToken(tokenBytes)
	if err != nil {
		return "", nil, err
	}
	token := callbackSecretPrefix + raw
	hash, err := HashCallbackSecret(authSecret, token)
	if err != nil {
		return "", nil, err
	}
	return token, hash, nil
}

func HashCallbackSecret(authSecret []byte, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, callbackSecretPrefix) {
		return nil, auth.ErrUnauthenticated
	}
	return auth.HashToken(authSecret, raw)
}

func IsCallbackSecret(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), callbackSecretPrefix)
}

func CallbackURL(publicURL *url.URL, tokenID string, callbackSecret string) string {
	path := "/api/waitpoints/tokens/" + url.PathEscape(tokenID) + "/callback/" + url.PathEscape(callbackSecret)
	if publicURL == nil {
		return path
	}
	return publicURL.ResolveReference(&url.URL{Path: path}).String()
}
