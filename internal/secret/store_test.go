package secret

import (
	"bytes"
	"encoding/base64"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestKeyFromBase64RequiresExactly32Bytes(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	decoded, err := KeyFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, key) {
		t.Fatal("decoded key changed")
	}
	for _, size := range []int{16, 31, 33} {
		if _, err := KeyFromBase64(
			base64.StdEncoding.EncodeToString(make([]byte, size)),
		); err == nil {
			t.Fatalf("%d-byte key was accepted", size)
		}
	}
}

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

func TestSecretNameContract(t *testing.T) {
	for _, name := range []string{"API_TOKEN", "config-json", "a.b", "0abc"} {
		if err := ValidateName(name); err != nil {
			t.Fatalf("ValidateName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "-bad", "bad/name", "bad name"} {
		if err := ValidateName(name); err == nil {
			t.Fatalf("ValidateName(%q) succeeded", name)
		}
	}
}

type cipherDatabase struct {
	db.Querier
}

func newCipherStore(t *testing.T, key []byte) *Store {
	t.Helper()
	store, err := New(&cipherDatabase{}, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
