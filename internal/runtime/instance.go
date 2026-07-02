package runtime

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

func NewInstanceToken() (string, error) {
	const byteLen = 32
	raw := make([]byte, byteLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate runtime instance token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if token == "" {
		return "", errors.New("runtime instance token is empty")
	}
	return token, nil
}
