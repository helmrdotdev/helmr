package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	APIKeyPrefix = "hlmr_sk_"
	apiKeyBytes  = 32
)

type GeneratedAPIKey struct {
	Raw       string
	KeyPrefix string
	TokenHash []byte
}

func GenerateAPIKey() (GeneratedAPIKey, error) {
	raw := make([]byte, apiKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("generate API key: %w", err)
	}
	key := APIKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return GeneratedAPIKey{
		Raw:       key,
		KeyPrefix: KeyPrefix(key),
		TokenHash: HashAPIKey(key),
	}, nil
}

func KeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= len(APIKeyPrefix)+8 {
		return key
	}
	return key[:len(APIKeyPrefix)+8]
}
