package workergroup

import (
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	hostSecretPrefix = "hlmr_wi_"
	hostSecretBytes  = 32
)

// generatedHostSecret is a new worker host secret: the raw value returned to
// the enrolling host once, its display prefix and its keyed hash.
type generatedHostSecret struct {
	Raw       string
	KeyPrefix string
	TokenHash []byte
}

func generateHostSecret(hashSecret []byte) (generatedHostSecret, error) {
	raw, err := auth.GenerateOpaque(hostSecretBytes)
	if err != nil {
		return generatedHostSecret{}, err
	}
	secret := hostSecretPrefix + raw
	hash, err := auth.HashToken(hashSecret, secret)
	if err != nil {
		return generatedHostSecret{}, err
	}
	return generatedHostSecret{
		Raw:       secret,
		KeyPrefix: hostSecretKeyPrefix(secret),
		TokenHash: hash,
	}, nil
}

func hostSecretKeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, hostSecretPrefix) || len(key) <= len(hostSecretPrefix)+8 {
		return key
	}
	return key[:len(hostSecretPrefix)+8]
}
