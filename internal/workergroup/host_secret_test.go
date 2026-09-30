package workergroup

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestGenerateHostSecret(t *testing.T) {
	hashSecret := make([]byte, auth.MACKeySize)
	for index := range hashSecret {
		hashSecret[index] = byte(index + 1)
	}
	generated, err := generateHostSecret(hashSecret)
	if err != nil {
		t.Fatal(err)
	}
	if hostSecretPrefix != "hlmr_wi_" {
		t.Fatalf("hostSecretPrefix = %q, want hlmr_wi_", hostSecretPrefix)
	}
	if !strings.HasPrefix(generated.Raw, hostSecretPrefix) {
		t.Fatalf("raw = %q, want prefix %q", generated.Raw, hostSecretPrefix)
	}
	randomPart := generated.Raw[len(hostSecretPrefix):]
	wantKeyPrefix := hostSecretPrefix + randomPart[:8]
	if generated.KeyPrefix != wantKeyPrefix {
		t.Fatalf("KeyPrefix = %q, want %q", generated.KeyPrefix, wantKeyPrefix)
	}
	hash, err := auth.HashToken(hashSecret, generated.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated.TokenHash) != string(hash) {
		t.Fatal("TokenHash is not the keyed hash of the raw secret")
	}
}
