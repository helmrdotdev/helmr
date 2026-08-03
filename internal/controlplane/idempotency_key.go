package controlplane

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxIdempotencyKeyLength = 512

func normalizeIdempotencyKey(key string) (string, error) {
	if !utf8.ValidString(key) {
		return "", fmt.Errorf("idempotency_key must be valid UTF-8")
	}
	if len(key) > maxIdempotencyKeyLength {
		return "", fmt.Errorf("idempotency_key must be at most %d bytes", maxIdempotencyKeyLength)
	}
	if strings.IndexByte(key, 0) >= 0 {
		return "", fmt.Errorf("idempotency_key cannot contain NUL")
	}
	if key != "" && strings.TrimSpace(key) != key {
		return "", fmt.Errorf("idempotency_key cannot begin or end with whitespace")
	}
	return key, nil
}
