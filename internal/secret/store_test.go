package secret

import (
	"bytes"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestAESGCMEnvelopeAuthenticatesSecretIdentity(t *testing.T) {
	store := newCipherStore(t, bytes.Repeat([]byte{1}, 32))
	store.rand = bytes.NewReader(make([]byte, store.encryption.NonceSize()))
	environmentID := uuid.New()
	secretID := uuid.New()
	versionID := uuid.New()
	plaintext := []byte("private value")

	encrypted, err := store.encrypt(environmentID, secretID, versionID, 1, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}
	version := db.SecretVersion{
		ID:         pgvalue.UUID(versionID),
		SecretID:   pgvalue.UUID(secretID),
		Version:    1,
		Nonce:      encrypted.nonce,
		Ciphertext: encrypted.ciphertext,
	}
	decrypted, err := store.decryptVersion(environmentID, secretID, versionID, version)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted value = %q", decrypted)
	}
	if _, err := store.decryptVersion(uuid.New(), secretID, versionID, version); err == nil {
		t.Fatal("ciphertext was accepted under a different environment")
	}
	if _, err := store.decryptVersion(environmentID, secretID, uuid.New(), version); err == nil {
		t.Fatal("ciphertext was accepted under a different version ID")
	}

	other := newCipherStore(t, bytes.Repeat([]byte{2}, 32))
	if _, err := other.decryptVersion(environmentID, secretID, versionID, version); err == nil {
		t.Fatal("ciphertext was accepted under a different key")
	}
}

type cipherDatabase struct {
	db.Querier
}

func newCipherStore(t *testing.T, key []byte) *Store {
	t.Helper()
	store, err := New(&cipherDatabase{}, nil, key, auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
