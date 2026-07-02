package token

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

func GenerateOpaque(byteLen int) (string, error) {
	if byteLen <= 0 {
		return "", errors.New("token byte length must be positive")
	}
	raw := make([]byte, byteLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
