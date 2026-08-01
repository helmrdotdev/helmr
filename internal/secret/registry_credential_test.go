package secret

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestOpenRegistryCredentialRequiresPinnedVersionOwnership(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	secretID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	store := newCipherStore(t, bytes.Repeat([]byte{9}, 32))
	store.rand = bytes.NewReader(make([]byte, store.encryption.NonceSize()))
	secretValue := db.Secret{
		ID:               pgvalue.UUID(secretID),
		EnvironmentID:    pgvalue.UUID(environmentID),
		State:            "active",
		CurrentVersionID: pgvalue.UUID(versionID),
	}
	encrypted, err := store.encrypt(
		environmentID,
		secretID,
		versionID,
		1,
		[]byte("registry-token"),
	)
	if err != nil {
		t.Fatalf("encrypt version: %v", err)
	}
	version := db.SecretVersion{
		ID:         pgvalue.UUID(versionID),
		SecretID:   pgvalue.UUID(secretID),
		Version:    1,
		Nonce:      encrypted.nonce,
		Ciphertext: encrypted.ciphertext,
	}

	value, err := store.OpenRegistryCredential(environmentID, secretValue, version)
	if err != nil {
		t.Fatalf("open registry credential: %v", err)
	}
	if string(value) != "registry-token" {
		t.Fatalf("value = %q", value)
	}
	clear(value)
	secretValue.CurrentVersionID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	rotatedValue, err := store.OpenRegistryCredential(environmentID, secretValue, version)
	if err != nil {
		t.Fatalf("open pinned registry credential after rotation: %v", err)
	}
	clear(rotatedValue)

	tests := []struct {
		name    string
		secret  db.Secret
		version db.SecretVersion
	}{
		{name: "revoked", secret: withSecretState(secretValue, "revoked"), version: version},
		{name: "wrong environment", secret: withSecretEnvironment(secretValue, uuid.Must(uuid.NewV7())), version: version},
		{name: "wrong Secret", secret: secretValue, version: withVersionSecret(version, uuid.Must(uuid.NewV7()))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := store.OpenRegistryCredential(environmentID, test.secret, test.version)
			if value != nil || !errors.Is(err, ErrRegistryCredentialUnavailable) {
				t.Fatalf("OpenRegistryCredential = %q, %v", value, err)
			}
		})
	}
}

func withSecretState(value db.Secret, state string) db.Secret {
	value.State = state
	return value
}

func withSecretEnvironment(value db.Secret, environmentID uuid.UUID) db.Secret {
	value.EnvironmentID = pgvalue.UUID(environmentID)
	return value
}

func withVersionSecret(value db.SecretVersion, secretID uuid.UUID) db.SecretVersion {
	value.SecretID = pgvalue.UUID(secretID)
	return value
}
