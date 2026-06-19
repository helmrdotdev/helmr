package publicaccess

import (
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	tokenPrefix = "hlmr_pat_"
	tokenBytes  = 32

	DefaultTokenTTL = 24 * time.Hour
)

func NewToken(authSecret []byte) (string, []byte, error) {
	raw, err := auth.GenerateOpaqueToken(tokenBytes)
	if err != nil {
		return "", nil, err
	}
	token := tokenPrefix + raw
	hash, err := HashToken(authSecret, token)
	if err != nil {
		return "", nil, err
	}
	return token, hash, nil
}

func HashToken(authSecret []byte, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, auth.ErrUnauthenticated
	}
	return auth.HashToken(authSecret, raw)
}

func IsToken(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), tokenPrefix)
}
