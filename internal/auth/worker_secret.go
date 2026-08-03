package auth

import (
	"strings"
)

const (
	WorkerInstanceSecretPrefix = "helmr_worker_instance_"
	workerSecretBytes          = 32
)

type GeneratedWorkerToken struct {
	Raw       string
	KeyPrefix string
	TokenHash []byte
}

func GenerateWorkerInstanceSecret(hashSecret []byte) (GeneratedWorkerToken, error) {
	raw, err := GenerateOpaque(workerSecretBytes)
	if err != nil {
		return GeneratedWorkerToken{}, err
	}
	workerToken := WorkerInstanceSecretPrefix + raw
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
	if !strings.HasPrefix(key, WorkerInstanceSecretPrefix) || len(key) <= len(WorkerInstanceSecretPrefix)+8 {
		return key
	}
	return key[:len(WorkerInstanceSecretPrefix)+8]
}
