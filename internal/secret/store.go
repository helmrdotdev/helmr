package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	keyIDPrefix              = "k_"
	maxWriteAttempts         = 3
	keyIDDeriveContext       = "helmr-secret-key-id:"
	envelopeDomain           = "helmr.secret-envelope.v0"
	valueAuthenticatorDomain = "helmr.secret-value.v0"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Store struct {
	db         db.Querier
	encryption Keyring
	hashes     keyedhash.Keyring
	rand       io.Reader
}

type Keyring struct {
	current secretKey
	keys    map[string]secretKey
	oldID   string
}

type secretKey struct {
	id   string
	aead cipher.AEAD
}

type KeyUsage struct {
	KeyID       string
	SecretCount int64
	Current     bool
	Old         bool
}

type AuthenticatorKeyUsage struct {
	Version     int32
	SecretCount int64
	Current     bool
}

type ReencryptBatchResult struct {
	Scanned     int
	Reencrypted int
	Skipped     int
	Failed      int
}

type ReauthenticateBatchResult struct {
	Scanned         int
	Reauthenticated int
	Skipped         int
	Failed          int
}

type UnavailableError struct {
	Err error
}

func (e UnavailableError) Error() string {
	return e.Err.Error()
}

func (e UnavailableError) Unwrap() error {
	return e.Err
}

func IsUnavailable(err error) bool {
	var unavailable UnavailableError
	return errors.As(err, &unavailable)
}

func New(database db.Querier, encryption Keyring, hashes keyedhash.Keyring) (*Store, error) {
	if database == nil {
		return nil, errors.New("secret database is required")
	}
	if encryption.current.id == "" {
		return nil, errors.New("secret encryption keyring current key is required")
	}
	if hashes.CurrentVersion() <= 0 {
		return nil, errors.New("secret authenticator keyring current key is required")
	}
	return &Store{db: database, encryption: encryption, hashes: hashes, rand: rand.Reader}, nil
}

func NewKeyring(current []byte, old []byte) (Keyring, error) {
	currentKey, err := newSecretKey(current)
	if err != nil {
		return Keyring{}, fmt.Errorf("configure current secret encryption key: %w", err)
	}
	keys := map[string]secretKey{currentKey.id: currentKey}
	keyring := Keyring{current: currentKey, keys: keys}
	if len(old) > 0 {
		oldKey, err := newSecretKey(old)
		if err != nil {
			return Keyring{}, fmt.Errorf("configure old secret encryption key: %w", err)
		}
		if oldKey.id == currentKey.id {
			return Keyring{}, errors.New("old secret encryption key must differ from current key")
		}
		keys[oldKey.id] = oldKey
		keyring.oldID = oldKey.id
	}
	return keyring, nil
}

func KeyringFromBase64(current string, old string) (Keyring, error) {
	currentKey, err := KeyFromBase64(current)
	if err != nil {
		return Keyring{}, err
	}
	var oldKey []byte
	if strings.TrimSpace(old) != "" {
		oldKey, err = KeyFromBase64(old)
		if err != nil {
			return Keyring{}, fmt.Errorf("decode old secret encryption key: %w", err)
		}
	}
	return NewKeyring(currentKey, oldKey)
}

func (k Keyring) CurrentKeyID() string {
	return k.current.id
}

func (k Keyring) OldKeyID() (string, bool) {
	return k.oldID, k.oldID != ""
}

func (k Keyring) key(keyID string) (secretKey, bool) {
	secretKey, ok := k.keys[keyID]
	return secretKey, ok
}

func newSecretKey(key []byte) (secretKey, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return secretKey{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return secretKey{}, fmt.Errorf("configure secret cipher: %w", err)
	}
	return secretKey{id: keyID(key), aead: aead}, nil
}

func KeyFromBase64(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("decode secret encryption key: %w", err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("secret encryption key must decode to 32 bytes, got %d", len(decoded))
	}
	return decoded, nil
}

func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

func ValidateName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("secret name %q must match %s", name, namePattern.String())
	}
	return nil
}

func (s *Store) Create(ctx context.Context, environmentID uuid.UUID, name string, value []byte, authenticatorKeyVersion int32) (db.Secret, error) {
	if err := ValidateName(name); err != nil {
		return db.Secret{}, err
	}
	secretID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	encrypted, err := s.encrypt(environmentID, secretID, versionID, 1, value)
	if err != nil {
		return db.Secret{}, err
	}
	authenticator, err := s.authenticator(environmentID, name, value, authenticatorKeyVersion)
	if err != nil {
		return db.Secret{}, err
	}
	row, err := s.db.CreateSecret(ctx, db.CreateSecretParams{
		ID:                      pgvalue.UUID(secretID),
		EnvironmentID:           pgvalue.UUID(environmentID),
		Name:                    name,
		VersionID:               pgvalue.UUID(versionID),
		KeyID:                   encrypted.keyID,
		Nonce:                   encrypted.nonce,
		Ciphertext:              encrypted.ciphertext,
		ValueAuthenticator:      authenticator,
		AuthenticatorKeyVersion: authenticatorKeyVersion,
	})
	if err != nil {
		return db.Secret{}, err
	}
	return secretFromCreate(row), nil
}

func (s *Store) Rotate(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, value []byte, authenticatorKeyVersion int32) (db.Secret, error) {
	var lastErr error
	for range maxWriteAttempts {
		record, err := s.db.GetSecret(ctx, db.GetSecretParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			ID:            pgvalue.UUID(secretID),
		})
		if err != nil {
			return db.Secret{}, err
		}
		if record.State != "active" {
			return db.Secret{}, UnavailableError{Err: fmt.Errorf("secret %q is %s", record.Name, record.State)}
		}
		current, err := s.db.GetCurrentSecretValue(ctx, db.GetCurrentSecretValueParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			SecretID:      record.ID,
		})
		if err != nil {
			return db.Secret{}, err
		}
		versionID := uuid.Must(uuid.NewV7())
		version := current.Version + 1
		encrypted, err := s.encrypt(environmentID, secretID, versionID, version, value)
		if err != nil {
			return db.Secret{}, err
		}
		authenticator, err := s.authenticator(environmentID, record.Name, value, authenticatorKeyVersion)
		if err != nil {
			return db.Secret{}, err
		}
		updated, err := s.db.RotateSecret(ctx, db.RotateSecretParams{
			VersionID:                pgvalue.UUID(versionID),
			Version:                  version,
			KeyID:                    encrypted.keyID,
			Nonce:                    encrypted.nonce,
			Ciphertext:               encrypted.ciphertext,
			ValueAuthenticator:       authenticator,
			AuthenticatorKeyVersion:  authenticatorKeyVersion,
			EnvironmentID:            pgvalue.UUID(environmentID),
			SecretID:                 record.ID,
			ExpectedStateVersion:     record.StateVersion,
			ExpectedCurrentVersionID: record.CurrentVersionID,
		})
		if err == nil {
			return updated, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			lastErr = err
			continue
		}
		return db.Secret{}, err
	}
	return db.Secret{}, fmt.Errorf("rotate secret after concurrent updates: %w", lastErr)
}

func (s *Store) Revoke(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID) (db.Secret, error) {
	var lastErr error
	for range maxWriteAttempts {
		record, err := s.db.GetSecret(ctx, db.GetSecretParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			ID:            pgvalue.UUID(secretID),
		})
		if err != nil {
			return db.Secret{}, err
		}
		if record.State == "revoked" {
			return record, nil
		}
		if record.State != "active" {
			return db.Secret{}, UnavailableError{Err: fmt.Errorf("secret %q is %s", record.Name, record.State)}
		}
		revoked, err := s.db.RevokeSecret(ctx, db.RevokeSecretParams{
			EnvironmentID:        pgvalue.UUID(environmentID),
			ID:                   record.ID,
			ExpectedStateVersion: record.StateVersion,
		})
		if err == nil {
			return revoked, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			lastErr = err
			continue
		}
		return db.Secret{}, err
	}
	return db.Secret{}, fmt.Errorf("revoke secret after concurrent updates: %w", lastErr)
}

func (s *Store) Put(ctx context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return db.Secret{}, err
	}
	return s.PutScoped(ctx, orgID, projectID, environmentID, name, value)
}

func (s *Store) PutScoped(ctx context.Context, _ uuid.UUID, _ uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	record, err := s.db.GetSecretByName(ctx, db.GetSecretByNameParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.Create(ctx, environmentID, name, value, s.hashes.CurrentVersion())
	case err != nil:
		return db.Secret{}, err
	case record.State != "active":
		return db.Secret{}, UnavailableError{Err: fmt.Errorf("secret %q is %s", name, record.State)}
	default:
		secretID, err := pgvalue.UUIDValue(record.ID)
		if err != nil {
			return db.Secret{}, err
		}
		return s.Rotate(ctx, environmentID, secretID, value, s.hashes.CurrentVersion())
	}
}

func (s *Store) CheckNames(ctx context.Context, orgID uuid.UUID, names []string) error {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return err
	}
	return s.CheckScopedNames(ctx, orgID, projectID, environmentID, names)
}

func (s *Store) ResolveNames(ctx context.Context, orgID uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.ResolveScopedNames(ctx, orgID, projectID, environmentID, names)
}

func (s *Store) CheckScopedNames(ctx context.Context, _ uuid.UUID, _ uuid.UUID, environmentID uuid.UUID, names []string) error {
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("invalid secret name: %w", err)
		}
		secret, version, err := s.currentByName(ctx, environmentID, name)
		if err != nil {
			return fmt.Errorf("secret %q is unavailable: %w", name, err)
		}
		if secret.State != "active" {
			return UnavailableError{Err: fmt.Errorf("secret %q is %s", name, secret.State)}
		}
		if _, ok := s.encryption.key(version.KeyID); !ok {
			return UnavailableError{Err: fmt.Errorf("secret %q uses unsupported key id %q", name, version.KeyID)}
		}
	}
	return nil
}

func (s *Store) ResolveScopedNames(ctx context.Context, _ uuid.UUID, _ uuid.UUID, environmentID uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	if len(names) == 0 {
		return api.ResolvedSecrets{}, nil
	}
	resolved := make(api.ResolvedSecrets, len(names))
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("invalid secret name: %w", err)}
		}
		secret, version, err := s.currentByName(ctx, environmentID, name)
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("resolve secret %q: %w", name, err)}
		}
		plaintext, err := s.decrypt(environmentID, secret, version)
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("resolve secret %q: %w", name, err)}
		}
		resolved[name] = plaintext
	}
	return resolved, nil
}

func (s *Store) currentByName(ctx context.Context, environmentID uuid.UUID, name string) (db.Secret, db.SecretVersion, error) {
	record, err := s.db.GetSecretByName(ctx, db.GetSecretByNameParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
	})
	if err != nil {
		return db.Secret{}, db.SecretVersion{}, err
	}
	if record.State != "active" {
		return record, db.SecretVersion{}, UnavailableError{Err: fmt.Errorf("secret %q is %s", name, record.State)}
	}
	version, err := s.db.GetCurrentSecretValue(ctx, db.GetCurrentSecretValueParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		SecretID:      record.ID,
	})
	if err != nil {
		return db.Secret{}, db.SecretVersion{}, err
	}
	return record, version, nil
}

func (s *Store) ReencryptBatch(ctx context.Context, fromKeyID string, limit int32) (ReencryptBatchResult, error) {
	fromKeyID = strings.TrimSpace(fromKeyID)
	if fromKeyID == "" {
		return ReencryptBatchResult{}, errors.New("source key id is required")
	}
	if fromKeyID == s.encryption.CurrentKeyID() {
		return ReencryptBatchResult{}, errors.New("source key id must not be the current key")
	}
	if limit <= 0 {
		return ReencryptBatchResult{}, errors.New("rotation batch limit must be positive")
	}
	sourceKey, ok := s.encryption.key(fromKeyID)
	if !ok {
		return ReencryptBatchResult{}, fmt.Errorf("source key id %q is not configured", fromKeyID)
	}
	rows, err := s.db.ListSecretVersionsByKeyID(ctx, db.ListSecretVersionsByKeyIDParams{
		KeyID:    fromKeyID,
		RowLimit: limit,
	})
	if err != nil {
		return ReencryptBatchResult{}, err
	}
	result := ReencryptBatchResult{Scanned: len(rows)}
	for _, row := range rows {
		environmentID, secretID, versionID, err := versionIDs(row.EnvironmentID, row.SecretID, row.VersionID)
		if err != nil {
			return result, err
		}
		plaintext, err := sourceKey.aead.Open(nil, row.Nonce, row.Ciphertext, envelopeAAD(environmentID, secretID, versionID, row.Version, row.KeyID))
		if err != nil {
			result.Failed++
			continue
		}
		encrypted, err := s.encrypt(environmentID, secretID, versionID, row.Version, plaintext)
		if err != nil {
			return result, err
		}
		updated, err := s.db.UpdateSecretVersionEnvelope(ctx, db.UpdateSecretVersionEnvelopeParams{
			NewKeyID:           encrypted.keyID,
			NewNonce:           encrypted.nonce,
			NewCiphertext:      encrypted.ciphertext,
			VersionID:          row.VersionID,
			PreviousKeyID:      row.KeyID,
			PreviousNonce:      row.Nonce,
			PreviousCiphertext: row.Ciphertext,
		})
		if err != nil {
			return result, err
		}
		if updated == 0 {
			result.Skipped++
			continue
		}
		result.Reencrypted++
	}
	return result, nil
}

func (s *Store) ReauthenticateBatch(ctx context.Context, fromVersion int32, limit int32) (ReauthenticateBatchResult, error) {
	if fromVersion <= 0 {
		return ReauthenticateBatchResult{}, errors.New("source authenticator key version must be positive")
	}
	if fromVersion == s.hashes.CurrentVersion() {
		return ReauthenticateBatchResult{}, errors.New("source authenticator key version must not be current")
	}
	if limit <= 0 {
		return ReauthenticateBatchResult{}, errors.New("rotation batch limit must be positive")
	}
	rows, err := s.db.ListSecretVersionsByAuthenticatorKeyVersion(ctx, db.ListSecretVersionsByAuthenticatorKeyVersionParams{
		AuthenticatorKeyVersion: fromVersion,
		RowLimit:                limit,
	})
	if err != nil {
		return ReauthenticateBatchResult{}, err
	}
	result := ReauthenticateBatchResult{Scanned: len(rows)}
	for _, row := range rows {
		environmentID, secretID, versionID, err := versionIDs(row.EnvironmentID, row.SecretID, row.VersionID)
		if err != nil {
			return result, err
		}
		key, ok := s.encryption.key(row.KeyID)
		if !ok {
			result.Failed++
			continue
		}
		plaintext, err := key.aead.Open(nil, row.Nonce, row.Ciphertext, envelopeAAD(environmentID, secretID, versionID, row.Version, row.KeyID))
		if err != nil {
			result.Failed++
			continue
		}
		authenticator, err := s.authenticator(environmentID, row.Name, plaintext, s.hashes.CurrentVersion())
		if err != nil {
			return result, err
		}
		updated, err := s.db.UpdateSecretVersionAuthenticator(ctx, db.UpdateSecretVersionAuthenticatorParams{
			NewValueAuthenticator:           authenticator,
			NewAuthenticatorKeyVersion:      s.hashes.CurrentVersion(),
			VersionID:                       row.VersionID,
			PreviousAuthenticatorKeyVersion: row.AuthenticatorKeyVersion,
			PreviousValueAuthenticator:      row.ValueAuthenticator,
		})
		if err != nil {
			return result, err
		}
		if updated == 0 {
			result.Skipped++
			continue
		}
		result.Reauthenticated++
	}
	return result, nil
}

func (s *Store) KeyUsage(ctx context.Context) ([]KeyUsage, error) {
	rows, err := s.db.ListSecretEncryptionKeyUsage(ctx)
	if err != nil {
		return nil, err
	}
	usage := make([]KeyUsage, 0, len(rows))
	for _, row := range rows {
		usage = append(usage, KeyUsage{
			KeyID:       row.KeyID,
			SecretCount: row.SecretCount,
			Current:     row.KeyID == s.encryption.CurrentKeyID(),
			Old:         row.KeyID == s.encryption.oldID,
		})
	}
	return usage, nil
}

func (s *Store) CountByKeyID(ctx context.Context, keyID string) (int64, error) {
	usage, err := s.db.ListSecretEncryptionKeyUsage(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range usage {
		if row.KeyID == keyID {
			return row.SecretCount, nil
		}
	}
	return 0, nil
}

func (s *Store) AuthenticatorKeyUsage(ctx context.Context) ([]AuthenticatorKeyUsage, error) {
	rows, err := s.db.ListSecretAuthenticatorKeyUsage(ctx)
	if err != nil {
		return nil, err
	}
	usage := make([]AuthenticatorKeyUsage, 0, len(rows))
	for _, row := range rows {
		usage = append(usage, AuthenticatorKeyUsage{
			Version:     row.AuthenticatorKeyVersion,
			SecretCount: row.SecretCount,
			Current:     row.AuthenticatorKeyVersion == s.hashes.CurrentVersion(),
		})
	}
	return usage, nil
}

func (s *Store) CountByAuthenticatorKeyVersion(ctx context.Context, version int32) (int64, error) {
	usage, err := s.db.ListSecretAuthenticatorKeyUsage(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range usage {
		if row.AuthenticatorKeyVersion == version {
			return row.SecretCount, nil
		}
	}
	return 0, nil
}

type encryptedSecret struct {
	keyID      string
	nonce      []byte
	ciphertext []byte
}

func (s *Store) encrypt(environmentID uuid.UUID, secretID uuid.UUID, versionID uuid.UUID, version int64, value []byte) (encryptedSecret, error) {
	key := s.encryption.current
	nonce := make([]byte, key.aead.NonceSize())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return encryptedSecret{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext := key.aead.Seal(nil, nonce, value, envelopeAAD(environmentID, secretID, versionID, version, key.id))
	return encryptedSecret{keyID: key.id, nonce: nonce, ciphertext: ciphertext}, nil
}

func (s *Store) decrypt(environmentID uuid.UUID, secret db.Secret, version db.SecretVersion) ([]byte, error) {
	key, ok := s.encryption.key(version.KeyID)
	if !ok {
		return nil, fmt.Errorf("unsupported key id %q", version.KeyID)
	}
	secretID, err := pgvalue.UUIDValue(secret.ID)
	if err != nil {
		return nil, err
	}
	versionID, err := pgvalue.UUIDValue(version.ID)
	if err != nil {
		return nil, err
	}
	return key.aead.Open(nil, version.Nonce, version.Ciphertext, envelopeAAD(environmentID, secretID, versionID, version.Version, version.KeyID))
}

func (s *Store) authenticator(environmentID uuid.UUID, name string, value []byte, version int32) ([]byte, error) {
	digest, err := s.hashes.Sum(version, valueAuthenticatorFrame(environmentID, name, value))
	if err != nil {
		return nil, err
	}
	return digest.Value[:], nil
}

func keyID(key []byte) string {
	sum := sha256.Sum256(append([]byte(keyIDDeriveContext), key...))
	return keyIDPrefix + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func envelopeAAD(environmentID uuid.UUID, secretID uuid.UUID, versionID uuid.UUID, version int64, keyID string) []byte {
	frame := make([]byte, 0, len(envelopeDomain)+1+16*3+8+2+len(keyID))
	frame = append(frame, envelopeDomain...)
	frame = append(frame, 0)
	frame = append(frame, environmentID[:]...)
	frame = append(frame, secretID[:]...)
	frame = append(frame, versionID[:]...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(version))
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(keyID)))
	frame = append(frame, keyID...)
	return frame
}

func valueAuthenticatorFrame(environmentID uuid.UUID, name string, value []byte) []byte {
	frame := make([]byte, 0, len(valueAuthenticatorDomain)+1+16+2+len(name)+8+len(value))
	frame = append(frame, valueAuthenticatorDomain...)
	frame = append(frame, 0)
	frame = append(frame, environmentID[:]...)
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(name)))
	frame = append(frame, name...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(value)))
	frame = append(frame, value...)
	return frame
}

func versionIDs(environment pgtype.UUID, secret pgtype.UUID, version pgtype.UUID) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	environmentID, err := pgvalue.UUIDValue(environment)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	secretID, err := pgvalue.UUIDValue(secret)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	versionID, err := pgvalue.UUIDValue(version)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	return environmentID, secretID, versionID, nil
}

func secretFromCreate(row db.CreateSecretRow) db.Secret {
	return db.Secret{
		ID:                   row.ID,
		EnvironmentID:        row.EnvironmentID,
		Name:                 row.Name,
		State:                row.State,
		StateVersion:         row.StateVersion,
		CurrentVersionID:     row.CurrentVersionID,
		RevocationGeneration: row.RevocationGeneration,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
		RevokedAt:            row.RevokedAt,
		DeletedAt:            row.DeletedAt,
	}
}

func (s *Store) defaultScope(ctx context.Context, orgID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	scope, err := s.db.GetDefaultProjectEnvironment(ctx, pgvalue.UUID(orgID))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	projectID, err := pgvalue.UUIDValue(scope.ProjectID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	environmentID, err := pgvalue.UUIDValue(scope.EnvironmentID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return projectID, environmentID, nil
}
