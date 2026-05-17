package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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

func GenerateOpaqueToken(byteLen int) (string, error) {
	if byteLen <= 0 {
		return "", errors.New("token byte length must be positive")
	}
	raw := make([]byte, byteLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
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
