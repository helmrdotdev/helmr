package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
)

const minTokenSecretBytes = 32

func ValidateTokenSecret(secret []byte) error {
	if len(secret) < minTokenSecretBytes {
		return fmt.Errorf("auth token signing key must be at least %d bytes", minTokenSecretBytes)
	}
	return nil
}

func HashToken(secret []byte, raw string) ([]byte, error) {
	if err := ValidateTokenSecret(secret); err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrUnauthenticated
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(raw))
	return mac.Sum(nil), nil
}
