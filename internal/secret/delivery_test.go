package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestOpenDeliveriesUsesRecordedVersionAfterRotation(t *testing.T) {
	environmentID := uuid.New()
	secretID := uuid.New()
	oldVersionID := uuid.New()
	currentVersionID := uuid.New()
	store := &Store{encryption: testCipher(t), rand: bytes.NewReader(make([]byte, 12))}
	encrypted, err := store.encrypt(environmentID, secretID, oldVersionID, 1, []byte("old-value"))
	if err != nil {
		t.Fatal(err)
	}
	secret := db.Secret{
		ID:               pgvalue.UUID(secretID),
		EnvironmentID:    pgvalue.UUID(environmentID),
		Status:           "active",
		CurrentVersionID: pgvalue.UUID(currentVersionID),
	}
	version := db.SecretVersion{
		ID:         pgvalue.UUID(oldVersionID),
		SecretID:   pgvalue.UUID(secretID),
		Version:    1,
		Nonce:      encrypted.nonce,
		Ciphertext: encrypted.ciphertext,
	}
	materials, err := store.OpenDeliveries(environmentID, []DeliveryEnvelope{{Mode: "raw",
		PlacementKind:   "env",
		PlacementTarget: "TOKEN",
		Secret:          secret,
		Version:         version,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(materials) != 1 ||
		materials[0].PlacementKind != "env" ||
		materials[0].PlacementTarget != "TOKEN" ||
		string(materials[0].Value) != "old-value" {
		t.Fatalf("materials = %+v", materials)
	}
}

func testCipher(t *testing.T) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	encryption, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return encryption
}

func TestOpenDeliveriesRejectsAuthorityMismatch(t *testing.T) {
	environmentID := uuid.New()
	secretID := uuid.New()
	versionID := uuid.New()
	store := &Store{}
	envelope := DeliveryEnvelope{
		Mode:            "raw",
		PlacementKind:   "env",
		PlacementTarget: "TOKEN",
		Secret: db.Secret{
			ID:            pgvalue.UUID(secretID),
			EnvironmentID: pgvalue.UUID(environmentID),
			Status:        "active",
		},
		Version: db.SecretVersion{
			ID:       pgvalue.UUID(versionID),
			SecretID: pgvalue.UUID(secretID),
		},
	}
	for _, test := range []struct {
		name string
		edit func(*DeliveryEnvelope)
	}{
		{name: "wrong environment", edit: func(value *DeliveryEnvelope) { value.Secret.EnvironmentID = pgvalue.UUID(uuid.New()) }},
		{name: "revoked Secret", edit: func(value *DeliveryEnvelope) { value.Secret.Status = "revoked" }},
		{name: "wrong Secret version owner", edit: func(value *DeliveryEnvelope) { value.Version.SecretID = pgvalue.UUID(uuid.New()) }},
		{name: "invalid placement", edit: func(value *DeliveryEnvelope) { value.PlacementKind = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := envelope
			test.edit(&value)
			_, err := store.OpenDeliveries(environmentID, []DeliveryEnvelope{value})
			if !errors.Is(err, ErrDeliveryUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
