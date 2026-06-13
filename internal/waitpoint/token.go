package waitpoint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	responseTokenPrefix = "hlmr_wpt_"
	responseTokenBytes  = 32

	DefaultResponseTokenTTL = 24 * time.Hour
)

func NewResponseToken(authSecret []byte) (string, []byte, error) {
	raw, err := auth.GenerateOpaqueToken(responseTokenBytes)
	if err != nil {
		return "", nil, err
	}
	token := responseTokenPrefix + raw
	hash, err := HashResponseToken(authSecret, token)
	if err != nil {
		return "", nil, err
	}
	return token, hash, nil
}

func HashResponseToken(authSecret []byte, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, responseTokenPrefix) {
		return nil, auth.ErrUnauthenticated
	}
	return auth.HashToken(authSecret, raw)
}

func NewEmailResponseToken(authSecret []byte, tokenID uuid.UUID) (string, []byte, error) {
	if tokenID == uuid.Nil {
		return "", nil, errors.New("waitpoint response token id is required")
	}
	if err := auth.ValidateTokenSecret(authSecret); err != nil {
		return "", nil, err
	}
	mac := hmac.New(sha256.New, authSecret)
	_, _ = mac.Write([]byte("helmr/waitpoint/email-response-token/v0/"))
	_, _ = mac.Write([]byte(tokenID.String()))
	raw := responseTokenPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	hash, err := HashResponseToken(authSecret, raw)
	if err != nil {
		return "", nil, err
	}
	return raw, hash, nil
}
