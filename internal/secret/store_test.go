package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStoreCreatesEncryptsAndResolves(t *testing.T) {
	database := newFakeSecretDB()
	store := newTestStore(t, database, makeKey(1), nil, 1)
	environmentID := uuid.Must(uuid.NewV7())
	created, err := store.create(t.Context(), environmentID, "github-token", []byte("secret-value"), 1)
	if err != nil {
		t.Fatal(err)
	}
	version := database.versions[created.CurrentVersionID.Bytes]
	if bytes.Equal(version.Ciphertext, []byte("secret-value")) {
		t.Fatal("secret was stored in plaintext")
	}
	if version.Version != 1 || version.AuthenticatorKeyVersion != 1 {
		t.Fatalf("version = %+v", version)
	}
	resolved, err := store.ResolveScopedNames(t.Context(), uuid.Nil, uuid.Nil, environmentID, []string{"github-token"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["github-token"]); got != "secret-value" {
		t.Fatalf("resolved = %q", got)
	}
}

func TestStoreRotatesLogicalVersion(t *testing.T) {
	database := newFakeSecretDB()
	store := newTestStore(t, database, makeKey(1), nil, 1)
	environmentID := uuid.Must(uuid.NewV7())
	created, err := store.create(t.Context(), environmentID, "API_TOKEN", []byte("first"), 1)
	if err != nil {
		t.Fatal(err)
	}
	secretID := pgvalue.MustUUIDValue(created.ID)
	rotated, err := store.rotate(t.Context(), environmentID, secretID, []byte("second"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.StateVersion != 2 || len(database.versions) != 2 {
		t.Fatalf("rotated = %+v, versions = %d", rotated, len(database.versions))
	}
	current := database.versions[rotated.CurrentVersionID.Bytes]
	if current.Version != 2 {
		t.Fatalf("version = %d, want 2", current.Version)
	}
	resolved, err := store.ResolveScopedNames(t.Context(), uuid.Nil, uuid.Nil, environmentID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "second" {
		t.Fatalf("resolved = %q", got)
	}
}

func TestStoreRevokesWholeSecret(t *testing.T) {
	database := newFakeSecretDB()
	store := newTestStore(t, database, makeKey(1), nil, 1)
	environmentID := uuid.Must(uuid.NewV7())
	created, err := store.create(t.Context(), environmentID, "API_TOKEN", []byte("value"), 1)
	if err != nil {
		t.Fatal(err)
	}
	revoked, changed, err := store.revoke(t.Context(), environmentID, pgvalue.MustUUIDValue(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("secret revocation did not report a state change")
	}
	if revoked.State != "revoked" || revoked.CurrentVersionID.Valid || revoked.RevocationGeneration != 1 {
		t.Fatalf("revoked = %+v", revoked)
	}
	if _, err := store.ResolveScopedNames(t.Context(), uuid.Nil, uuid.Nil, environmentID, []string{"API_TOKEN"}); !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestStoreReencryptsEveryRetainedVersion(t *testing.T) {
	database := newFakeSecretDB()
	oldStore := newTestStore(t, database, makeKey(1), nil, 1)
	environmentID := uuid.Must(uuid.NewV7())
	created, err := oldStore.create(t.Context(), environmentID, "API_TOKEN", []byte("first"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.rotate(t.Context(), environmentID, pgvalue.MustUUIDValue(created.ID), []byte("second"), 1); err != nil {
		t.Fatal(err)
	}
	rotating := newTestStore(t, database, makeKey(2), makeKey(1), 1)
	oldKeyID, ok := rotating.encryption.OldKeyID()
	if !ok {
		t.Fatal("old key missing")
	}
	result, err := rotating.ReencryptBatch(t.Context(), oldKeyID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reencrypted != 2 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	for _, version := range database.versions {
		if version.KeyID != rotating.encryption.CurrentKeyID() {
			t.Fatalf("key id = %q", version.KeyID)
		}
	}
	resolved, err := rotating.ResolveScopedNames(t.Context(), uuid.Nil, uuid.Nil, environmentID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "second" {
		t.Fatalf("resolved = %q", got)
	}
}

func TestStoreReauthenticatesWithoutChangingLogicalVersion(t *testing.T) {
	database := newFakeSecretDB()
	store := newTestStore(t, database, makeKey(1), nil, 2)
	environmentID := uuid.Must(uuid.NewV7())
	created, err := store.create(t.Context(), environmentID, "API_TOKEN", []byte("value"), 1)
	if err != nil {
		t.Fatal(err)
	}
	before := database.versions[created.CurrentVersionID.Bytes]
	result, err := store.reauthenticateBatch(t.Context(), database, 2, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	after := database.versions[created.CurrentVersionID.Bytes]
	if result.Reauthenticated != 1 || after.AuthenticatorKeyVersion != 2 {
		t.Fatalf("result = %+v, version = %+v", result, after)
	}
	if before.Version != after.Version || before.ID != after.ID || !bytes.Equal(before.Ciphertext, after.Ciphertext) {
		t.Fatal("reauthentication changed logical or encrypted value identity")
	}
	if bytes.Equal(before.ValueAuthenticator, after.ValueAuthenticator) {
		t.Fatal("authenticator did not change")
	}
}

func TestValueAuthenticatorFrameIsScopeBoundAndInjective(t *testing.T) {
	environmentA := uuid.Must(uuid.NewV7())
	environmentB := uuid.Must(uuid.NewV7())
	a := valueAuthenticatorFrame(environmentA, "ab", []byte("c"))
	b := valueAuthenticatorFrame(environmentA, "a", []byte("bc"))
	c := valueAuthenticatorFrame(environmentB, "ab", []byte("c"))
	if bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("distinct value tuples produced the same frame")
	}
}

func TestStoreRejectsUnsupportedEncryptionKey(t *testing.T) {
	database := newFakeSecretDB()
	oldStore := newTestStore(t, database, makeKey(1), nil, 1)
	environmentID := uuid.Must(uuid.NewV7())
	if _, err := oldStore.create(t.Context(), environmentID, "API_TOKEN", []byte("value"), 1); err != nil {
		t.Fatal(err)
	}
	currentStore := newTestStore(t, database, makeKey(2), nil, 1)
	_, err := currentStore.ResolveScopedNames(t.Context(), uuid.Nil, uuid.Nil, environmentID, []string{"API_TOKEN"})
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestStoreRejectsMissingRetainedAuthenticatorKey(t *testing.T) {
	database := newFakeSecretDB()
	database.currentHashVersion = 3

	encryption, err := NewKeyring(makeKey(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := keyedhash.New(map[int32][]byte{
		1: makeKey(10),
		2: makeKey(11),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), database, nil, encryption, hashes); err == nil {
		t.Fatal("expected missing retained authenticator key error")
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
	secrets            map[[16]byte]db.Secret
	byName             map[string][16]byte
	versions           map[[16]byte]db.SecretVersion
	currentHashVersion int32
}

func newFakeSecretDB() *fakeSecretDB {
	return &fakeSecretDB{
		secrets:            map[[16]byte]db.Secret{},
		byName:             map[string][16]byte{},
		versions:           map[[16]byte]db.SecretVersion{},
		currentHashVersion: 1,
	}
}

func (f *fakeSecretDB) ListLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error) {
	hashes, err := keyedhash.New(map[int32][]byte{
		1: makeKey(10),
		2: makeKey(11),
	})
	if err != nil {
		return nil, err
	}
	rows := make([]db.LookupHmacVersion, 0, 2)
	for _, version := range []int32{1, 2} {
		fingerprint, err := hashes.Fingerprint(version)
		if err != nil {
			return nil, err
		}
		rows = append(rows, db.LookupHmacVersion{
			Version:        version,
			KeyFingerprint: fingerprint[:],
			IsCurrent:      version == f.currentHashVersion,
		})
	}
	if f.currentHashVersion > 2 {
		rows = append(rows, db.LookupHmacVersion{
			Version:        f.currentHashVersion,
			KeyFingerprint: bytes.Repeat([]byte{byte(f.currentHashVersion)}, 32),
			IsCurrent:      true,
		})
		for index := range rows[:2] {
			rows[index].IsCurrent = false
		}
	}
	return rows, nil
}

func (f *fakeSecretDB) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     pgvalue.UUID(dbtest.DefaultOrgID),
		EnvironmentID: pgvalue.UUID(dbtest.DefaultOrgID),
	}, nil
}

func (f *fakeSecretDB) CreateSecret(_ context.Context, arg db.CreateSecretParams) (db.CreateSecretRow, error) {
	nameKey := scopedName(arg.EnvironmentID, arg.Name)
	if _, ok := f.byName[nameKey]; ok {
		return db.CreateSecretRow{}, errors.New("duplicate secret")
	}
	record := db.Secret{
		ID:                   arg.ID,
		EnvironmentID:        arg.EnvironmentID,
		Name:                 arg.Name,
		State:                "active",
		StateVersion:         1,
		CurrentVersionID:     arg.VersionID,
		RevocationGeneration: 0,
	}
	version := db.SecretVersion{
		ID:                      arg.VersionID,
		SecretID:                arg.ID,
		Version:                 1,
		KeyID:                   arg.KeyID,
		Nonce:                   bytes.Clone(arg.Nonce),
		Ciphertext:              bytes.Clone(arg.Ciphertext),
		ValueAuthenticator:      bytes.Clone(arg.ValueAuthenticator),
		AuthenticatorKeyVersion: arg.AuthenticatorKeyVersion,
	}
	f.secrets[arg.ID.Bytes] = record
	f.versions[arg.VersionID.Bytes] = version
	f.byName[nameKey] = arg.ID.Bytes
	return createRow(record), nil
}

func (f *fakeSecretDB) GetSecret(_ context.Context, arg db.GetSecretParams) (db.Secret, error) {
	record, ok := f.secrets[arg.ID.Bytes]
	if !ok || record.EnvironmentID != arg.EnvironmentID {
		return db.Secret{}, pgx.ErrNoRows
	}
	return record, nil
}

func (f *fakeSecretDB) GetSecretByName(_ context.Context, arg db.GetSecretByNameParams) (db.Secret, error) {
	id, ok := f.byName[scopedName(arg.EnvironmentID, arg.Name)]
	if !ok {
		return db.Secret{}, pgx.ErrNoRows
	}
	return f.secrets[id], nil
}

func (f *fakeSecretDB) GetCurrentSecretValue(_ context.Context, arg db.GetCurrentSecretValueParams) (db.SecretVersion, error) {
	secret, ok := f.secrets[arg.SecretID.Bytes]
	if !ok || secret.EnvironmentID != arg.EnvironmentID || secret.State != "active" {
		return db.SecretVersion{}, pgx.ErrNoRows
	}
	return f.versions[secret.CurrentVersionID.Bytes], nil
}

func (f *fakeSecretDB) RotateSecret(_ context.Context, arg db.RotateSecretParams) (db.Secret, error) {
	record, ok := f.secrets[arg.SecretID.Bytes]
	if !ok || record.EnvironmentID != arg.EnvironmentID || record.State != "active" ||
		record.StateVersion != arg.ExpectedStateVersion || record.CurrentVersionID != arg.ExpectedCurrentVersionID {
		return db.Secret{}, pgx.ErrNoRows
	}
	f.versions[arg.VersionID.Bytes] = db.SecretVersion{
		ID:                      arg.VersionID,
		SecretID:                arg.SecretID,
		Version:                 arg.Version,
		KeyID:                   arg.KeyID,
		Nonce:                   bytes.Clone(arg.Nonce),
		Ciphertext:              bytes.Clone(arg.Ciphertext),
		ValueAuthenticator:      bytes.Clone(arg.ValueAuthenticator),
		AuthenticatorKeyVersion: arg.AuthenticatorKeyVersion,
	}
	record.CurrentVersionID = arg.VersionID
	record.StateVersion++
	f.secrets[arg.SecretID.Bytes] = record
	return record, nil
}

func (f *fakeSecretDB) RevokeSecret(_ context.Context, arg db.RevokeSecretParams) (db.Secret, error) {
	record, ok := f.secrets[arg.ID.Bytes]
	if !ok || record.EnvironmentID != arg.EnvironmentID || record.State != "active" || record.StateVersion != arg.ExpectedStateVersion {
		return db.Secret{}, pgx.ErrNoRows
	}
	record.State = "revoked"
	record.StateVersion++
	record.CurrentVersionID = pgtype.UUID{}
	record.RevocationGeneration++
	f.secrets[arg.ID.Bytes] = record
	return record, nil
}

func (f *fakeSecretDB) ListSecretVersionsByKeyID(_ context.Context, arg db.ListSecretVersionsByKeyIDParams) ([]db.ListSecretVersionsByKeyIDRow, error) {
	rows := make([]db.ListSecretVersionsByKeyIDRow, 0)
	for _, version := range f.versions {
		if version.KeyID != arg.KeyID || int32(len(rows)) >= arg.RowLimit {
			continue
		}
		secret := f.secrets[version.SecretID.Bytes]
		rows = append(rows, versionByKeyRow(secret, version))
	}
	return rows, nil
}

func (f *fakeSecretDB) UpdateSecretVersionEnvelope(_ context.Context, arg db.UpdateSecretVersionEnvelopeParams) (int64, error) {
	version, ok := f.versions[arg.VersionID.Bytes]
	if !ok || version.KeyID != arg.PreviousKeyID || !bytes.Equal(version.Nonce, arg.PreviousNonce) || !bytes.Equal(version.Ciphertext, arg.PreviousCiphertext) {
		return 0, nil
	}
	version.KeyID = arg.NewKeyID
	version.Nonce = bytes.Clone(arg.NewNonce)
	version.Ciphertext = bytes.Clone(arg.NewCiphertext)
	f.versions[arg.VersionID.Bytes] = version
	return 1, nil
}

func (f *fakeSecretDB) ListSecretVersionsByAuthenticatorKeyVersion(_ context.Context, arg db.ListSecretVersionsByAuthenticatorKeyVersionParams) ([]db.ListSecretVersionsByAuthenticatorKeyVersionRow, error) {
	rows := make([]db.ListSecretVersionsByAuthenticatorKeyVersionRow, 0)
	for _, version := range f.versions {
		if version.AuthenticatorKeyVersion != arg.AuthenticatorKeyVersion || int32(len(rows)) >= arg.RowLimit {
			continue
		}
		secret := f.secrets[version.SecretID.Bytes]
		rows = append(rows, db.ListSecretVersionsByAuthenticatorKeyVersionRow{
			SecretID:                secret.ID,
			EnvironmentID:           secret.EnvironmentID,
			Name:                    secret.Name,
			VersionID:               version.ID,
			Version:                 version.Version,
			KeyID:                   version.KeyID,
			Nonce:                   bytes.Clone(version.Nonce),
			Ciphertext:              bytes.Clone(version.Ciphertext),
			ValueAuthenticator:      bytes.Clone(version.ValueAuthenticator),
			AuthenticatorKeyVersion: version.AuthenticatorKeyVersion,
		})
	}
	return rows, nil
}

func (f *fakeSecretDB) UpdateSecretVersionAuthenticator(_ context.Context, arg db.UpdateSecretVersionAuthenticatorParams) (int64, error) {
	version, ok := f.versions[arg.VersionID.Bytes]
	if !ok || version.AuthenticatorKeyVersion != arg.PreviousAuthenticatorKeyVersion || !bytes.Equal(version.ValueAuthenticator, arg.PreviousValueAuthenticator) {
		return 0, nil
	}
	version.ValueAuthenticator = bytes.Clone(arg.NewValueAuthenticator)
	version.AuthenticatorKeyVersion = arg.NewAuthenticatorKeyVersion
	f.versions[arg.VersionID.Bytes] = version
	return 1, nil
}

func (f *fakeSecretDB) ListSecretEncryptionKeyUsage(context.Context) ([]db.ListSecretEncryptionKeyUsageRow, error) {
	counts := map[string]int64{}
	for _, version := range f.versions {
		counts[version.KeyID]++
	}
	rows := make([]db.ListSecretEncryptionKeyUsageRow, 0, len(counts))
	for keyID, count := range counts {
		rows = append(rows, db.ListSecretEncryptionKeyUsageRow{KeyID: keyID, SecretCount: count})
	}
	return rows, nil
}

func (f *fakeSecretDB) ListSecretAuthenticatorKeyUsage(context.Context) ([]db.ListSecretAuthenticatorKeyUsageRow, error) {
	counts := map[int32]int64{}
	for _, version := range f.versions {
		counts[version.AuthenticatorKeyVersion]++
	}
	rows := make([]db.ListSecretAuthenticatorKeyUsageRow, 0, len(counts))
	for version, count := range counts {
		rows = append(rows, db.ListSecretAuthenticatorKeyUsageRow{
			AuthenticatorKeyVersion: version,
			SecretCount:             count,
		})
	}
	return rows, nil
}

func versionByKeyRow(secret db.Secret, version db.SecretVersion) db.ListSecretVersionsByKeyIDRow {
	return db.ListSecretVersionsByKeyIDRow{
		SecretID:                secret.ID,
		EnvironmentID:           secret.EnvironmentID,
		Name:                    secret.Name,
		VersionID:               version.ID,
		Version:                 version.Version,
		KeyID:                   version.KeyID,
		Nonce:                   bytes.Clone(version.Nonce),
		Ciphertext:              bytes.Clone(version.Ciphertext),
		ValueAuthenticator:      bytes.Clone(version.ValueAuthenticator),
		AuthenticatorKeyVersion: version.AuthenticatorKeyVersion,
	}
}

func createRow(secret db.Secret) db.CreateSecretRow {
	return db.CreateSecretRow{
		ID:                   secret.ID,
		EnvironmentID:        secret.EnvironmentID,
		Name:                 secret.Name,
		State:                secret.State,
		StateVersion:         secret.StateVersion,
		CurrentVersionID:     secret.CurrentVersionID,
		RevocationGeneration: secret.RevocationGeneration,
	}
}

func scopedName(environmentID pgtype.UUID, name string) string {
	return string(environmentID.Bytes[:]) + "\x00" + name
}

func newTestStore(t *testing.T, database *fakeSecretDB, current []byte, old []byte, currentHashVersion int32) *Store {
	t.Helper()
	database.currentHashVersion = currentHashVersion
	encryption, err := NewKeyring(current, old)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := keyedhash.New(map[int32][]byte{
		1: makeKey(10),
		2: makeKey(11),
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.Context(), database, nil, encryption, hashes)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func makeKey(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed
	}
	return key
}
