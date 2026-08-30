package secret

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLockAttemptDeliveryReturnsExactRecordedVersion(t *testing.T) {
	runID := pgvalue.UUID(uuid.New())
	workspaceID := pgvalue.UUID(uuid.New())
	environmentID := pgvalue.UUID(uuid.New())
	secretID := pgvalue.UUID(uuid.New())
	oldVersionID := pgvalue.UUID(uuid.New())
	currentVersionID := pgvalue.UUID(uuid.New())
	secret := db.Secret{
		ID:                   secretID,
		EnvironmentID:        environmentID,
		State:                "active",
		CurrentVersionID:     currentVersionID,
		RevocationGeneration: 3,
	}
	store := &fakeDeliveryStore{
		rows: []db.LockAttemptSecretDeliveryRow{
			deliveryRow(runID, workspaceID, secret, oldVersionID, 3, "env", "TOKEN"),
			deliveryRow(runID, workspaceID, secret, oldVersionID, 3, "file", "/run/helmr/token"),
		},
		version: db.SecretVersion{ID: oldVersionID, SecretID: secretID, Version: 1},
	}

	envelopes, err := LockAttemptDelivery(t.Context(), store, runID, 2, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 2 ||
		envelopes[0].Version.ID != oldVersionID ||
		envelopes[1].Version.ID != oldVersionID ||
		store.versionReads != 1 {
		t.Fatalf("envelopes=%+v version_reads=%d", envelopes, store.versionReads)
	}
	if store.params.RunID != runID ||
		store.params.AttemptNumber != (pgtype.Int4{Int32: 2, Valid: true}) ||
		store.params.WorkspaceID != workspaceID {
		t.Fatalf("params = %+v", store.params)
	}
}

func TestLockAttemptDeliveryRejectsIncompleteOrRevokedAuthority(t *testing.T) {
	runID := pgvalue.UUID(uuid.New())
	workspaceID := pgvalue.UUID(uuid.New())
	environmentID := pgvalue.UUID(uuid.New())
	secretID := pgvalue.UUID(uuid.New())
	versionID := pgvalue.UUID(uuid.New())
	active := db.Secret{
		ID:                   secretID,
		EnvironmentID:        environmentID,
		State:                "active",
		CurrentVersionID:     versionID,
		RevocationGeneration: 4,
	}
	for _, test := range []struct {
		name string
		edit func(*db.LockAttemptSecretDeliveryRow)
	}{
		{name: "missing resolution", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.ResolutionID = pgtype.UUID{} }},
		{name: "wrong Run", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.ResolutionRunID = pgvalue.UUID(uuid.New()) }},
		{name: "wrong Attempt", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.ResolutionAttemptNumber.Int32++ }},
		{name: "revocation generation changed", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.Secret.RevocationGeneration++ }},
		{name: "revoked", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.Secret.State = "revoked" }},
		{name: "wrong Workspace", edit: func(row *db.LockAttemptSecretDeliveryRow) { row.WorkspaceSecret.WorkspaceID = pgvalue.UUID(uuid.New()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := deliveryRow(runID, workspaceID, active, versionID, 4, "env", "TOKEN")
			test.edit(&row)
			store := &fakeDeliveryStore{
				rows:    []db.LockAttemptSecretDeliveryRow{row},
				version: db.SecretVersion{ID: versionID, SecretID: secretID, Version: 1},
			}
			_, err := LockAttemptDelivery(t.Context(), store, runID, 2, workspaceID)
			if !errors.Is(err, ErrDeliveryUnavailable) {
				t.Fatalf("error = %v", err)
			}
			if store.versionReads != 0 {
				t.Fatalf("version reads = %d, want 0", store.versionReads)
			}
		})
	}
}

func TestLockAttemptDeliveryAllowsEmptyWorkspaceSecretSet(t *testing.T) {
	envelopes, err := LockAttemptDelivery(
		t.Context(),
		&fakeDeliveryStore{},
		pgvalue.UUID(uuid.New()),
		1,
		pgvalue.UUID(uuid.New()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 0 {
		t.Fatalf("envelopes = %+v", envelopes)
	}
}

func TestLockAttemptDeliveryRejectsPlacementOverflow(t *testing.T) {
	runID := pgvalue.UUID(uuid.New())
	workspaceID := pgvalue.UUID(uuid.New())
	environmentID := pgvalue.UUID(uuid.New())
	rows := make([]db.LockAttemptSecretDeliveryRow, maxWorkspaceSecretPlacements+1)
	for index := range rows {
		secretID := pgvalue.UUID(uuid.New())
		versionID := pgvalue.UUID(uuid.New())
		rows[index] = deliveryRow(runID, workspaceID, db.Secret{
			ID:                   secretID,
			EnvironmentID:        environmentID,
			State:                "active",
			CurrentVersionID:     versionID,
			RevocationGeneration: 1,
		}, versionID, 1, "env", "TOKEN")
	}
	store := &fakeDeliveryStore{rows: rows}
	_, err := LockAttemptDelivery(t.Context(), store, runID, 2, workspaceID)
	if !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if store.versionReads != 0 {
		t.Fatalf("version reads = %d, want 0", store.versionReads)
	}
}

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
		State:            "active",
		CurrentVersionID: pgvalue.UUID(currentVersionID),
	}
	version := db.SecretVersion{
		ID:         pgvalue.UUID(oldVersionID),
		SecretID:   pgvalue.UUID(secretID),
		Version:    1,
		Nonce:      encrypted.nonce,
		Ciphertext: encrypted.ciphertext,
	}
	materials, err := store.OpenDeliveries(environmentID, []DeliveryEnvelope{{
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
		PlacementKind:   "env",
		PlacementTarget: "TOKEN",
		Secret: db.Secret{
			ID:            pgvalue.UUID(secretID),
			EnvironmentID: pgvalue.UUID(environmentID),
			State:         "active",
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
		{name: "revoked Secret", edit: func(value *DeliveryEnvelope) { value.Secret.State = "revoked" }},
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

func deliveryRow(
	runID pgtype.UUID,
	workspaceID pgtype.UUID,
	secret db.Secret,
	versionID pgtype.UUID,
	revocationGeneration int64,
	placementKind string,
	placementTarget string,
) db.LockAttemptSecretDeliveryRow {
	return db.LockAttemptSecretDeliveryRow{
		WorkspaceSecret: db.WorkspaceSecret{
			WorkspaceID:     workspaceID,
			EnvironmentID:   secret.EnvironmentID,
			PlacementKind:   placementKind,
			PlacementTarget: placementTarget,
			SecretID:        secret.ID,
		},
		Secret:                         secret,
		ResolutionID:                   pgvalue.UUID(uuid.New()),
		ResolutionRunID:                runID,
		ResolutionAttemptNumber:        pgtype.Int4{Int32: 2, Valid: true},
		ResolutionSecretVersionID:      versionID,
		ResolutionRevocationGeneration: pgtype.Int8{Int64: revocationGeneration, Valid: true},
	}
}

type fakeDeliveryStore struct {
	params       db.LockAttemptSecretDeliveryParams
	rows         []db.LockAttemptSecretDeliveryRow
	version      db.SecretVersion
	versionReads int
}

func (s *fakeDeliveryStore) LockAttemptSecretDelivery(
	_ context.Context,
	params db.LockAttemptSecretDeliveryParams,
) ([]db.LockAttemptSecretDeliveryRow, error) {
	s.params = params
	return s.rows, nil
}

func (s *fakeDeliveryStore) GetSecretVersion(
	_ context.Context,
	_ db.GetSecretVersionParams,
) (db.SecretVersion, error) {
	s.versionReads++
	return s.version, nil
}
