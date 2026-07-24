package token

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

func TestCredentialKeysDeriveStableSeparatedCredentials(t *testing.T) {
	var key [CredentialKeySize]byte
	for i := range key {
		key[i] = byte(i + 1)
	}
	id := CredentialKeyIDForKey(key)
	keys, err := NewCredentialKeys(id.String(), map[string][]byte{id.String(): key[:]})
	if err != nil {
		t.Fatal(err)
	}
	tokenID := uuid.MustParse("019c1234-5678-7abc-8def-0123456789ab")
	first, err := keys.DeriveActive(tokenID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := keys.Derive(id, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if first.CallbackSecret != second.CallbackSecret ||
		first.PublicAccessToken != second.PublicAccessToken ||
		!bytes.Equal(first.CallbackFingerprint, second.CallbackFingerprint) ||
		!bytes.Equal(first.PublicAccessHash, second.PublicAccessHash) {
		t.Fatal("credential derivation is not stable")
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

func TestCredentialKeysFromBase64JSONRejectsRetargetedID(t *testing.T) {
	var key [CredentialKeySize]byte
	key[0] = 1
	id := CredentialKeyIDForKey(key)
	var replacement [CredentialKeySize]byte
	replacement[0] = 2
	raw := `{"` + id.String() + `":"` + base64.StdEncoding.EncodeToString(replacement[:]) + `"}`
	if _, err := CredentialKeysFromBase64JSON(id.String(), raw); err == nil {
		t.Fatal("retargeted key ID was accepted")
	}
}
