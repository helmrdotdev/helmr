package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const (
	keyIDPrefix        = "k_"
	maxWriteAttempts   = 3
	keyIDDeriveContext = "helmr-secret-key-id:"
	aadVersion         = "1"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Store struct {
	db      db.Querier
	keyring Keyring
	rand    io.Reader
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

type ReencryptBatchResult struct {
	Scanned     int
	Reencrypted int
	Skipped     int
	Failed      int
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

func New(database db.Querier, keyring Keyring) (*Store, error) {
	if database == nil {
		return nil, errors.New("secret database is required")
	}
	if keyring.current.id == "" {
		return nil, errors.New("secret keyring current key is required")
	}
	return &Store{db: database, keyring: keyring, rand: rand.Reader}, nil
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

func (s *Store) Put(ctx context.Context, cellID string, orgID uuid.UUID, name string, value []byte) (db.Secret, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return db.Secret{}, err
	}
	return s.PutScoped(ctx, cellID, orgID, projectID, environmentID, name, value)
}

func (s *Store) PutScoped(ctx context.Context, cellID string, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	if err := ValidateName(name); err != nil {
		return db.Secret{}, err
	}
	var lastErr error
	for range maxWriteAttempts {
		record, err := s.scopedSecret(ctx, cellID, orgID, projectID, environmentID, name)
		previousVersion := int32(0)
		version := int32(1)
		if err == nil {
			previousVersion = record.Version
			version = record.Version + 1
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return db.Secret{}, err
		}
		encrypted, err := s.encrypt(orgID, projectID, environmentID, name, version, value)
		if err != nil {
			return db.Secret{}, err
		}
		updated, err := s.db.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
			ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:           pgvalue.UUID(orgID),
			CellID:          cellID,
			ProjectID:       pgvalue.UUID(projectID),
			EnvironmentID:   pgvalue.UUID(environmentID),
			Name:            name,
			Version:         version,
			KeyID:           encrypted.keyID,
			Nonce:           encrypted.nonce,
			Ciphertext:      encrypted.ciphertext,
			PreviousVersion: previousVersion,
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
	return db.Secret{}, fmt.Errorf("write secret %q after concurrent updates: %w", name, lastErr)
}

func (s *Store) CheckNames(ctx context.Context, cellID string, orgID uuid.UUID, names []string) error {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return err
	}
	return s.CheckScopedNames(ctx, cellID, orgID, projectID, environmentID, names)
}

func (s *Store) ResolveNames(ctx context.Context, cellID string, orgID uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.ResolveScopedNames(ctx, cellID, orgID, projectID, environmentID, names)
}

func (s *Store) CheckScopedNames(ctx context.Context, cellID string, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("invalid secret name: %w", err)
		}
		record, err := s.scopedSecret(ctx, cellID, orgID, projectID, environmentID, name)
		if err != nil {
			return fmt.Errorf("secret %q is unavailable: %w", name, err)
		}
		if _, ok := s.keyring.key(record.KeyID); !ok {
			return UnavailableError{Err: fmt.Errorf("secret %q uses unsupported key id %q", name, record.KeyID)}
		}
	}
	return nil
}

func (s *Store) ResolveScopedNames(ctx context.Context, cellID string, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	if len(names) == 0 {
		return api.ResolvedSecrets{}, nil
	}
	resolved := make(api.ResolvedSecrets, len(names))
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("invalid secret name: %w", err)}
		}
		record, err := s.scopedSecret(ctx, cellID, orgID, projectID, environmentID, name)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, UnavailableError{Err: fmt.Errorf("resolve secret %q: %w", name, err)}
			}
			return nil, fmt.Errorf("resolve secret %q: %w", name, err)
		}
		key, ok := s.keyring.key(record.KeyID)
		if !ok {
			return nil, UnavailableError{Err: fmt.Errorf("secret %q uses unsupported key id %q", name, record.KeyID)}
		}
		plaintext, err := key.aead.Open(nil, record.Nonce, record.Ciphertext, scopedAdditionalData(orgID, projectID, environmentID, record.Name, record.Version, record.KeyID))
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("decrypt secret %q: %w", name, err)}
		}
		resolved[name] = plaintext
	}
	return resolved, nil
}

func (s *Store) scopedSecret(ctx context.Context, cellID string, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string) (db.Secret, error) {
	record, err := s.db.GetScopedSecretByName(ctx, db.GetScopedSecretByNameParams{
		OrgID:         pgvalue.UUID(orgID),
		CellID:        cellID,
		ProjectID:     pgvalue.UUID(projectID),
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
	})
	if err != nil {
		return db.Secret{}, err
	}
	return record, nil
}

func (s *Store) ReencryptBatch(ctx context.Context, fromKeyID string, limit int32) (ReencryptBatchResult, error) {
	fromKeyID = strings.TrimSpace(fromKeyID)
	if fromKeyID == "" {
		return ReencryptBatchResult{}, errors.New("source key id is required")
	}
	if fromKeyID == s.keyring.CurrentKeyID() {
		return ReencryptBatchResult{}, errors.New("source key id must not be the current key")
	}
	if limit <= 0 {
		return ReencryptBatchResult{}, errors.New("rotation batch limit must be positive")
	}
	sourceKey, ok := s.keyring.key(fromKeyID)
	if !ok {
		return ReencryptBatchResult{}, fmt.Errorf("source key id %q is not configured", fromKeyID)
	}
	rows, err := s.db.ListSecretsByKeyIDForRotation(ctx, db.ListSecretsByKeyIDForRotationParams{
		KeyID:    fromKeyID,
		RowLimit: limit,
	})
	if err != nil {
		return ReencryptBatchResult{}, err
	}
	result := ReencryptBatchResult{Scanned: len(rows)}
	for _, row := range rows {
		orgID, err := pgvalue.UUIDValue(row.OrgID)
		if err != nil {
			return result, err
		}
		projectID, err := pgvalue.UUIDValue(row.ProjectID)
		if err != nil {
			return result, err
		}
		environmentID, err := pgvalue.UUIDValue(row.EnvironmentID)
		if err != nil {
			return result, err
		}
		plaintext, err := sourceKey.aead.Open(nil, row.Nonce, row.Ciphertext, scopedAdditionalData(orgID, projectID, environmentID, row.Name, row.Version, row.KeyID))
		if err != nil {
			result.Failed++
			continue
		}
		newVersion := row.Version + 1
		encrypted, err := s.encrypt(orgID, projectID, environmentID, row.Name, newVersion, plaintext)
		if err != nil {
			return result, err
		}
		updated, err := s.db.UpdateSecretCiphertextForRotation(ctx, db.UpdateSecretCiphertextForRotationParams{
			NewVersion:      newVersion,
			NewKeyID:        encrypted.keyID,
			Nonce:           encrypted.nonce,
			Ciphertext:      encrypted.ciphertext,
			ID:              row.ID,
			PreviousKeyID:   row.KeyID,
			PreviousVersion: row.Version,
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

func (s *Store) KeyUsage(ctx context.Context) ([]KeyUsage, error) {
	rows, err := s.db.ListSecretKeyUsage(ctx)
	if err != nil {
		return nil, err
	}
	usage := make([]KeyUsage, 0, len(rows))
	for _, row := range rows {
		usage = append(usage, KeyUsage{
			KeyID:       row.KeyID,
			SecretCount: row.SecretCount,
			Current:     row.KeyID == s.keyring.CurrentKeyID(),
			Old:         row.KeyID == s.keyring.oldID,
		})
	}
	return usage, nil
}

func (s *Store) CountByKeyID(ctx context.Context, keyID string) (int64, error) {
	return s.db.CountSecretsByKeyID(ctx, keyID)
}

type encryptedSecret struct {
	keyID      string
	nonce      []byte
	ciphertext []byte
}

func (s *Store) encrypt(orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, version int32, value []byte) (encryptedSecret, error) {
	key := s.keyring.current
	nonce := make([]byte, key.aead.NonceSize())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return encryptedSecret{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext := key.aead.Seal(nil, nonce, value, scopedAdditionalData(orgID, projectID, environmentID, name, version, key.id))
	return encryptedSecret{keyID: key.id, nonce: nonce, ciphertext: ciphertext}, nil
}

func keyID(key []byte) string {
	sum := sha256.Sum256(append([]byte(keyIDDeriveContext), key...))
	return keyIDPrefix + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func scopedAdditionalData(orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, version int32, keyID string) []byte {
	return []byte(aadVersion + "\x00" + orgID.String() + "\x00" + projectID.String() + "\x00" + environmentID.String() + "\x00" + name + "\x00" + fmt.Sprint(version) + "\x00" + keyID)
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
