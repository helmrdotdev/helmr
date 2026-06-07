package secret

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStoreEncryptsAndResolvesNames(t *testing.T) {
	key, err := KeyFromBase64(base64.StdEncoding.EncodeToString(makeKey(1)))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := NewKeyring(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeSecretDB{}
	store, err := New(database, keyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := store.Put(context.Background(), orgID, "github-token", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckNames(context.Background(), orgID, []string{"github-token"}); err != nil {
		t.Fatal(err)
	}
	if string(database.record.Ciphertext) == "secret-value" {
		t.Fatal("secret was stored in plaintext")
	}
	if database.record.Version != 1 {
		t.Fatalf("version = %d, want 1", database.record.Version)
	}
	if database.record.KeyID != keyring.CurrentKeyID() {
		t.Fatalf("key id = %q, want %q", database.record.KeyID, keyring.CurrentKeyID())
	}
	resolved, err := store.ResolveNames(context.Background(), orgID, []string{"github-token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved["github-token"]) != "secret-value" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestStoreIncrementsVersionOnUpdate(t *testing.T) {
	keyring := newTestKeyring(t, makeKey(1), nil)
	database := &fakeSecretDB{}
	store, err := New(database, keyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := store.Put(context.Background(), orgID, "API_TOKEN", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), orgID, "API_TOKEN", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if database.record.Version != 2 {
		t.Fatalf("version = %d, want 2", database.record.Version)
	}
	resolved, err := store.ResolveNames(context.Background(), orgID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "second" {
		t.Fatalf("resolved = %q, want second", got)
	}
}

func TestStoreResolvesOldKeyDuringRotation(t *testing.T) {
	oldKey := makeKey(1)
	currentKey := makeKey(2)
	oldKeyring := newTestKeyring(t, oldKey, nil)
	database := &fakeSecretDB{}
	oldStore, err := New(database, oldKeyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := oldStore.Put(context.Background(), orgID, "API_TOKEN", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	rotatingKeyring := newTestKeyring(t, currentKey, oldKey)
	rotatingStore, err := New(database, rotatingKeyring)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := rotatingStore.ResolveNames(context.Background(), orgID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "secret-value" {
		t.Fatalf("resolved = %q, want secret-value", got)
	}
}

func TestStoreReencryptBatchMovesOldKeyToCurrentKey(t *testing.T) {
	oldKey := makeKey(1)
	currentKey := makeKey(2)
	oldKeyring := newTestKeyring(t, oldKey, nil)
	database := &fakeSecretDB{}
	oldStore, err := New(database, oldKeyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := oldStore.Put(context.Background(), orgID, "API_TOKEN", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	previousVersion := database.record.Version
	rotatingKeyring := newTestKeyring(t, currentKey, oldKey)
	rotatingStore, err := New(database, rotatingKeyring)
	if err != nil {
		t.Fatal(err)
	}
	oldKeyID, ok := rotatingKeyring.OldKeyID()
	if !ok {
		t.Fatal("old key id missing")
	}
	result, err := rotatingStore.ReencryptBatch(context.Background(), oldKeyID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 1 || result.Reencrypted != 1 || result.Skipped != 0 {
		t.Fatalf("result = %+v", result)
	}
	if database.record.KeyID != rotatingKeyring.CurrentKeyID() {
		t.Fatalf("key id = %q, want current %q", database.record.KeyID, rotatingKeyring.CurrentKeyID())
	}
	if database.record.Version != previousVersion+1 {
		t.Fatalf("version = %d, want %d", database.record.Version, previousVersion+1)
	}
	resolved, err := rotatingStore.ResolveNames(context.Background(), orgID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "secret-value" {
		t.Fatalf("resolved = %q, want secret-value", got)
	}
}

func TestStoreRejectsUnsupportedKeyID(t *testing.T) {
	oldKeyring := newTestKeyring(t, makeKey(1), nil)
	database := &fakeSecretDB{}
	oldStore, err := New(database, oldKeyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := oldStore.Put(context.Background(), orgID, "API_TOKEN", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	currentStore, err := New(database, newTestKeyring(t, makeKey(2), nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = currentStore.ResolveNames(context.Background(), orgID, []string{"API_TOKEN"})
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestStoreCheckNamesRejectsUnsupportedKeyID(t *testing.T) {
	oldKeyring := newTestKeyring(t, makeKey(1), nil)
	database := &fakeSecretDB{}
	oldStore, err := New(database, oldKeyring)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := oldStore.Put(context.Background(), orgID, "API_TOKEN", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	currentStore, err := New(database, newTestKeyring(t, makeKey(2), nil))
	if err != nil {
		t.Fatal(err)
	}
	err = currentStore.CheckNames(context.Background(), orgID, []string{"API_TOKEN"})
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestValidateNameCorpus(t *testing.T) {
	valid := []string{"config-json", "0abc", "a.b", "A_B", "CON"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Fatalf("ValidateName(%q) = %v", name, err)
		}
	}
	invalid := []string{"", "-x", "_x", "bad/name", "bad name", strings.Repeat("a", 129)}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Fatalf("ValidateName(%q) succeeded", name)
		}
	}
}

func TestKeyFromBase64RequiresAES256Key(t *testing.T) {
	_, err := KeyFromBase64(base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if err == nil {
		t.Fatal("expected short key error")
	}
}

type fakeSecretDB struct {
	db.Querier
	record db.Secret
}

func (f *fakeSecretDB) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     ids.ToPG(ids.DefaultOrgID),
		EnvironmentID: ids.ToPG(ids.DefaultOrgID),
	}, nil
}

func (f *fakeSecretDB) UpsertScopedSecret(_ context.Context, arg db.UpsertScopedSecretParams) (db.Secret, error) {
	if f.record.ID.Valid && f.record.Version != arg.PreviousVersion {
		return db.Secret{}, pgx.ErrNoRows
	}
	f.record = db.Secret{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		Name:          arg.Name,
		Version:       arg.Version,
		KeyID:         arg.KeyID,
		Nonce:         arg.Nonce,
		Ciphertext:    arg.Ciphertext,
	}
	return f.record, nil
}

func (f *fakeSecretDB) GetScopedSecretByName(_ context.Context, arg db.GetScopedSecretByNameParams) (db.Secret, error) {
	if f.record.OrgID != arg.OrgID || f.record.ProjectID != arg.ProjectID || f.record.EnvironmentID != arg.EnvironmentID || f.record.Name != arg.Name {
		return db.Secret{}, pgx.ErrNoRows
	}
	return f.record, nil
}

func (f *fakeSecretDB) ListSecretsByKeyIDForRotation(_ context.Context, arg db.ListSecretsByKeyIDForRotationParams) ([]db.Secret, error) {
	if f.record.KeyID != arg.KeyID || arg.RowLimit == 0 {
		return nil, nil
	}
	return []db.Secret{f.record}, nil
}

func (f *fakeSecretDB) UpdateSecretCiphertextForRotation(_ context.Context, arg db.UpdateSecretCiphertextForRotationParams) (int64, error) {
	if f.record.ID != arg.ID || f.record.KeyID != arg.PreviousKeyID || f.record.Version != arg.PreviousVersion {
		return 0, nil
	}
	f.record.Version = arg.NewVersion
	f.record.KeyID = arg.NewKeyID
	f.record.Nonce = arg.Nonce
	f.record.Ciphertext = arg.Ciphertext
	return 1, nil
}

func (f *fakeSecretDB) ListSecretKeyUsage(context.Context) ([]db.ListSecretKeyUsageRow, error) {
	if f.record.KeyID == "" {
		return nil, nil
	}
	return []db.ListSecretKeyUsageRow{{KeyID: f.record.KeyID, SecretCount: 1}}, nil
}

func (f *fakeSecretDB) CountSecretsByKeyID(_ context.Context, keyID string) (int64, error) {
	if f.record.KeyID == keyID {
		return 1, nil
	}
	return 0, nil
}

func newTestKeyring(t *testing.T, current []byte, old []byte) Keyring {
	t.Helper()
	keyring, err := NewKeyring(current, old)
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func makeKey(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed
	}
	return key
}
