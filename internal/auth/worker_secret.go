package auth

import "strings"

const (
	WorkerRegistrationTokenPrefix = "helmr_register_"
	WorkerSecretPrefix            = "helmr_secret_"
	workerSecretBytes             = 32
)

type GeneratedWorkerToken struct {
	Raw       string
	KeyPrefix string
	TokenHash []byte
}

func GenerateWorkerRegistrationToken(hashSecret []byte) (GeneratedWorkerToken, error) {
	return generatePrefixedWorkerToken(hashSecret, WorkerRegistrationTokenPrefix)
}

func GenerateWorkerSecret(hashSecret []byte) (GeneratedWorkerToken, error) {
	return generatePrefixedWorkerToken(hashSecret, WorkerSecretPrefix)
}

func WorkerKeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	prefix, ok := workerTokenPrefix(key)
	if !ok || len(key) <= len(prefix)+8 {
		return key
	}
	return key[:len(prefix)+8]
}

func generatePrefixedWorkerToken(hashSecret []byte, prefix string) (GeneratedWorkerToken, error) {
	raw, err := GenerateOpaqueToken(workerSecretBytes)
	if err != nil {
		return GeneratedWorkerToken{}, err
	}
	token := prefix + raw
	hash, err := HashToken(hashSecret, token)
	if err != nil {
		return GeneratedWorkerToken{}, err
	}
	return GeneratedWorkerToken{
		Raw:       token,
		KeyPrefix: WorkerKeyPrefix(token),
		TokenHash: hash,
	}, nil
}

func workerTokenPrefix(key string) (string, bool) {
	key = strings.TrimSpace(key)
	switch {
	case strings.HasPrefix(key, WorkerRegistrationTokenPrefix):
		return WorkerRegistrationTokenPrefix, true
	case strings.HasPrefix(key, WorkerSecretPrefix):
		return WorkerSecretPrefix, true
	default:
		return "", false
	}
}
