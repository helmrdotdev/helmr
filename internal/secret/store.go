package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretname"
	"github.com/jackc/pgx/v5"
)

const envelopeDomain = "helmr.secret-envelope.v0"

type Store struct {
	db         db.Querier
	tx         db.TxBeginner
	encryption cipher.AEAD
	rand       io.Reader
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

func New(database db.Querier, transactions db.TxBeginner, encryptionKey []byte) (*Store, error) {
	if database == nil {
		return nil, errors.New("secret database is required")
	}
	if len(encryptionKey) != 32 {
		return nil, fmt.Errorf("secret encryption key must be 32 bytes, got %d", len(encryptionKey))
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure secret cipher: %w", err)
	}
	encryption, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("configure secret cipher: %w", err)
	}
	return &Store{
		db:         database,
		tx:         transactions,
		encryption: encryption,
		rand:       rand.Reader,
	}, nil
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

func (s *Store) create(ctx context.Context, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	if err := secretname.Validate(name); err != nil {
		return db.Secret{}, err
	}
	secretID := uuid.NewV7()
	versionID := uuid.NewV7()
	encrypted, err := s.encrypt(environmentID, secretID, versionID, 1, value)
	if err != nil {
		return db.Secret{}, err
	}
	row, err := s.db.CreateSecret(ctx, db.CreateSecretParams{
		ID:            pgvalue.UUID(secretID),
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
		VersionID:     pgvalue.UUID(versionID),
		Nonce:         encrypted.nonce,
		Ciphertext:    encrypted.ciphertext,
	})
	if err != nil {
		return db.Secret{}, err
	}
	return secretFromCreate(row), nil
}

// ErrMutationConflict means a retry key was reused with different content.
var ErrMutationConflict = errors.New("secret retry identity conflicts with its original request")
var ErrInvalidMutation = errors.New("invalid Secret mutation")

func mutationKey(raw string, required bool) (string, error) {
	key := strings.TrimSpace(raw)
	if !required && key == "" {
		return "", nil
	}
	if key == "" || len(key) > 512 || !utf8.ValidString(key) || strings.ContainsFunc(key, unicode.IsControl) {
		return "", ErrInvalidMutation
	}
	return key, nil
}

// Create serializes Environment-owned name and retry identities. Its first
// encrypted version is the immutable value receipt, even after rotation/revocation.
func (s *Store) Create(ctx context.Context, env uuid.UUID, name string, value []byte, retryKey string) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	if env == uuid.Nil() {
		return db.GetSecretSnapshotRow{}, ErrInvalidMutation
	}
	if err := secretname.Validate(name); err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	key, err := mutationKey(retryKey, false)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	var result db.GetSecretSnapshotRow
	err = db.RunTx(ctx, s.tx, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&locked); err != nil {
			return err
		}
		q := db.New(tx)
		claims, _ := idempotency.TransactionFor(tx)
		var acquired idempotency.Result
		if key != "" {
			request, err := idempotency.NewSecretCreateRequest(env, name, key)
			if err != nil {
				return err
			}
			acquired, err = claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
			if !acquired.New {
				id := pgvalue.MustUUIDValue(acquired.Claim.SecretID)
				if err = s.compareVersion(ctx, tx, env, id, pgvalue.MustUUIDValue(acquired.Claim.SecretVersionID), value); err != nil {
					return err
				}
				result, err = q.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{EnvironmentID: pgvalue.UUID(env), ID: acquired.Claim.SecretID})
				return err
			}
		}
		bound := *s
		bound.db = q
		record, err := bound.create(ctx, env, name, value)
		if err != nil {
			return err
		}
		if key != "" {
			_, err = claims.Complete(ctx, acquired.Claim, idempotency.Target{SecretID: pgvalue.MustUUIDValue(record.ID), SecretVersionID: pgvalue.MustUUIDValue(record.CurrentVersionID)}, []byte(`{}`))
			if err != nil {
				return err
			}
		}
		result, err = q.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{EnvironmentID: pgvalue.UUID(env), ID: record.ID})
		return err
	})
	return result, err
}

// Rotate binds retries to the exact encrypted version and never rewrites pins.
func (s *Store) Rotate(ctx context.Context, env, id uuid.UUID, value []byte, retryKey string) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	key, err := mutationKey(retryKey, true)
	if err != nil || env == uuid.Nil() || id == uuid.Nil() {
		return db.GetSecretSnapshotRow{}, ErrInvalidMutation
	}
	var result db.GetSecretSnapshotRow
	err = db.RunTx(ctx, s.tx, func(tx pgx.Tx) error {
		claims, _ := idempotency.TransactionFor(tx)
		request, err := idempotency.NewSecretRotateRequest(env, id, key)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, request)
		if err != nil {
			return err
		}
		record, err := lockMutationSecret(ctx, tx, env, id)
		if err != nil {
			return err
		}
		q := db.New(tx)
		if !acquired.New {
			if err = s.compareVersion(ctx, tx, env, id, pgvalue.MustUUIDValue(acquired.Claim.SecretVersionID), value); err != nil {
				return err
			}
			result, err = q.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{EnvironmentID: pgvalue.UUID(env), ID: record.ID})
			return err
		}
		if record.Status != "active" {
			return UnavailableError{Err: fmt.Errorf("secret %q is %s", record.Name, record.Status)}
		}
		current, err := q.GetCurrentSecretValue(ctx, db.GetCurrentSecretValueParams{EnvironmentID: pgvalue.UUID(env), SecretID: record.ID})
		if err != nil {
			return err
		}
		versionID := uuid.NewV7()
		version := current.Version + 1
		encrypted, err := s.encrypt(env, id, versionID, version, value)
		if err != nil {
			return err
		}
		updated, err := q.RotateSecret(ctx, db.RotateSecretParams{VersionID: pgvalue.UUID(versionID), Version: version, Nonce: encrypted.nonce, Ciphertext: encrypted.ciphertext, EnvironmentID: pgvalue.UUID(env), SecretID: record.ID, ExpectedRevision: record.Revision, ExpectedCurrentVersionID: record.CurrentVersionID})
		if err != nil {
			return err
		}
		if _, err = claims.Complete(ctx, acquired.Claim, idempotency.Target{SecretID: id, SecretVersionID: versionID}, []byte(`{}`)); err != nil {
			return err
		}
		result, err = q.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{EnvironmentID: pgvalue.UUID(env), ID: updated.ID})
		return err
	})
	return result, err
}

// Revoke records intent, never a claim that physical execution has stopped.
func (s *Store) Revoke(ctx context.Context, env, id uuid.UUID, retryKey string) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	key, err := mutationKey(retryKey, false)
	if err != nil || env == uuid.Nil() || id == uuid.Nil() {
		return db.GetSecretSnapshotRow{}, ErrInvalidMutation
	}
	var result db.GetSecretSnapshotRow
	err = db.RunTx(ctx, s.tx, func(tx pgx.Tx) error {
		claims, _ := idempotency.TransactionFor(tx)
		var acquired idempotency.Result
		if key != "" {
			request, err := idempotency.NewSecretRevokeRequest(env, id, key)
			if err != nil {
				return err
			}
			acquired, err = claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
		}
		record, err := lockMutationSecret(ctx, tx, env, id)
		if err != nil {
			return err
		}
		q := db.New(tx)
		if record.Status != "revoked" {
			if _, err = q.RevokeSecret(ctx, db.RevokeSecretParams{EnvironmentID: pgvalue.UUID(env), ID: record.ID, ExpectedRevision: record.Revision}); err != nil {
				return err
			}
			payload, err := json.Marshal(secretRevocationPayload{EnvironmentID: env.String(), SecretID: id.String(), RevocationGeneration: record.RevocationGeneration + 1})
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO control_outbox(id,topic,payload) VALUES($1,'secret.revoked',$2)`, uuid.NewV7(), payload); err != nil {
				return err
			}

		}
		if key != "" && acquired.New {
			if _, err = claims.Complete(ctx, acquired.Claim, idempotency.Target{SecretID: id}, []byte(`{}`)); err != nil {
				return err
			}
		}
		result, err = q.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{EnvironmentID: pgvalue.UUID(env), ID: record.ID})
		return err
	})
	return result, err
}
func lockMutationSecret(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) (db.Secret, error) {
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT s.id FROM secrets s JOIN environments e ON e.id=s.environment_id WHERE s.environment_id=$1 AND s.id=$2 AND e.retired_at IS NULL FOR UPDATE OF s`, env, id).Scan(&locked); err != nil {
		return db.Secret{}, err
	}
	return db.New(tx).GetSecret(ctx, db.GetSecretParams{EnvironmentID: pgvalue.UUID(env), ID: pgvalue.UUID(id)})
}
func (s *Store) compareVersion(ctx context.Context, tx pgx.Tx, env, id, versionID uuid.UUID, value []byte) error {
	var version db.SecretVersion
	if err := tx.QueryRow(ctx, `SELECT version,nonce,ciphertext FROM secret_versions WHERE secret_id=$1 AND id=$2`, id, versionID).Scan(&version.Version, &version.Nonce, &version.Ciphertext); err != nil {
		return err
	}
	plaintext, err := s.decryptVersion(env, id, versionID, version)
	if err != nil {
		return fmt.Errorf("decrypt original Secret value: %w", err)
	}
	defer clear(plaintext)
	if subtle.ConstantTimeCompare(plaintext, value) != 1 {
		return ErrMutationConflict
	}
	return nil
}

type encryptedSecret struct {
	nonce      []byte
	ciphertext []byte
}

func (s *Store) encrypt(environmentID uuid.UUID, secretID uuid.UUID, versionID uuid.UUID, version int64, value []byte) (encryptedSecret, error) {
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return encryptedSecret{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext := s.encryption.Seal(nil, nonce, value, envelopeAAD(environmentID, secretID, versionID, version))
	return encryptedSecret{nonce: nonce, ciphertext: ciphertext}, nil
}

func (s *Store) decrypt(environmentID uuid.UUID, secret db.Secret, version db.SecretVersion) ([]byte, error) {
	secretID, err := pgvalue.UUIDValue(secret.ID)
	if err != nil {
		return nil, err
	}
	versionID, err := pgvalue.UUIDValue(version.ID)
	if err != nil {
		return nil, err
	}
	return s.decryptVersion(environmentID, secretID, versionID, version)
}

func (s *Store) decryptVersion(
	environmentID uuid.UUID,
	secretID uuid.UUID,
	versionID uuid.UUID,
	version db.SecretVersion,
) ([]byte, error) {
	return s.encryption.Open(
		nil,
		version.Nonce,
		version.Ciphertext,
		envelopeAAD(environmentID, secretID, versionID, version.Version),
	)
}

func envelopeAAD(environmentID uuid.UUID, secretID uuid.UUID, versionID uuid.UUID, version int64) []byte {
	frame := make([]byte, 0, len(envelopeDomain)+1+16*3+8)
	frame = append(frame, envelopeDomain...)
	frame = append(frame, 0)
	frame = append(frame, environmentID[:]...)
	frame = append(frame, secretID[:]...)
	frame = append(frame, versionID[:]...)
	frame = binary.BigEndian.AppendUint64(frame, uint64(version))
	return frame
}

func secretFromCreate(row db.CreateSecretRow) db.Secret {
	return db.Secret(row)
}
