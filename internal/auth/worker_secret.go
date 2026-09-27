package auth

import (
	"strings"
)

const (
	WorkerHostSecretPrefix = "hlmr_wi_"
	workerSecretBytes      = 32
)

type GeneratedWorkerToken struct {
	Raw       string
	KeyPrefix string
	TokenHash []byte
}

func GenerateWorkerHostSecret(hashSecret []byte) (GeneratedWorkerToken, error) {
	raw, err := GenerateOpaque(workerSecretBytes)
	if err != nil {
		return GeneratedWorkerToken{}, err
	}
	workerToken := WorkerHostSecretPrefix + raw
	hash, err := HashToken(hashSecret, workerToken)
	if err != nil {
		return GeneratedWorkerToken{}, err
	}
	return GeneratedWorkerToken{
		Raw:       workerToken,
		KeyPrefix: WorkerKeyPrefix(workerToken),
		TokenHash: hash,
	}, nil
}

func WorkerKeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, WorkerHostSecretPrefix) || len(key) <= len(WorkerHostSecretPrefix)+8 {
		return key
	}
	return key[:len(WorkerHostSecretPrefix)+8]
}
