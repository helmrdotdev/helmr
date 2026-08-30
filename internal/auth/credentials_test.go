package auth

import (
	"bytes"
	"strings"
	"testing"

	"uuid"
)

func TestCredentialKeyDerivesStableSeparatedCredentials(t *testing.T) {
	key := make([]byte, CredentialKeySize)
	for index := range key {
		key[index] = byte(index + 1)
	}
	credentialKey, err := NewCredentialKey(key)
	if err != nil {
		t.Fatal(err)
	}
	tokenID := uuid.MustParse("019c1234-5678-7abc-8def-0123456789ab")
	first, err := credentialKey.Derive(tokenID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := credentialKey.Derive(tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if first.CallbackSecret != second.CallbackSecret ||
		first.PublicAccessToken != second.PublicAccessToken ||
		!bytes.Equal(first.CallbackFingerprint, second.CallbackFingerprint) ||
		!bytes.Equal(first.PublicAccessHash, second.PublicAccessHash) {
		t.Fatal("credential derivation is not stable")
	}
	if !strings.HasPrefix(first.PublicAccessToken, "hlmr_pub_") {
		t.Fatalf("public access token = %q", first.PublicAccessToken)
	}
	if first.CallbackSecret == first.PublicAccessToken ||
		bytes.Equal(first.CallbackFingerprint, first.PublicAccessHash) {
		t.Fatal("callback and bearer credentials are not domain separated")
	}
	if !bytes.Equal(HashCredential(first.CallbackSecret), first.CallbackFingerprint) ||
		!bytes.Equal(HashCredential(first.PublicAccessToken), first.PublicAccessHash) {
		t.Fatal("credential hashes do not match")
	}
}

func TestCredentialKeyRejectsInvalidAuthority(t *testing.T) {
	for _, size := range []int{0, CredentialKeySize - 1, CredentialKeySize + 1} {
		if _, err := NewCredentialKey(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d-byte key", size)
		}
	}
	if _, err := (CredentialKey{}).Derive(uuid.New()); err == nil {
		t.Fatal("zero-value key was accepted")
	}
	key, err := NewCredentialKey(make([]byte, CredentialKeySize))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := key.Derive(uuid.Nil()); err == nil {
		t.Fatal("nil Token ID was accepted")
	}
}

func TestGenerateWorkerInstanceSecret(t *testing.T) {
	hashSecret := make([]byte, MACKeySize)
	for index := range hashSecret {
		hashSecret[index] = byte(index + 1)
	}
	generated, err := GenerateWorkerInstanceSecret(hashSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(generated.Raw, WorkerInstanceSecretPrefix) {
		t.Fatalf("raw = %q, want prefix %q", generated.Raw, WorkerInstanceSecretPrefix)
	}
	if WorkerInstanceSecretPrefix != "hlmr_wi_" {
		t.Fatalf("WorkerInstanceSecretPrefix = %q, want hlmr_wi_", WorkerInstanceSecretPrefix)
	}
	randomPart := generated.Raw[len(WorkerInstanceSecretPrefix):]
	wantKeyPrefix := WorkerInstanceSecretPrefix + randomPart[:8]
	if generated.KeyPrefix != wantKeyPrefix {
		t.Fatalf("KeyPrefix = %q, want %q", generated.KeyPrefix, wantKeyPrefix)
	}
	if len(generated.TokenHash) == 0 {
		t.Fatal("TokenHash is empty")
	}
}
