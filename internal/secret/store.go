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
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxWriteAttempts = 3
	envelopeDomain   = "helmr.secret-envelope.v0"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Store struct {
	db         db.Querier
	tx         transactionBeginner
	encryption cipher.AEAD
	rand       io.Reader
}

type transactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
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

func New(database db.Querier, transactions transactionBeginner, encryptionKey []byte) (*Store, error) {
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

func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

func ValidateName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("secret name %q must match %s", name, namePattern.String())
	}
	return nil
}

func (s *Store) create(ctx context.Context, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	if err := ValidateName(name); err != nil {
		return db.Secret{}, err
	}
	secretID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
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

func (s *Store) rotate(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, value []byte) (db.Secret, error) {
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
		updated, err := s.db.RotateSecret(ctx, db.RotateSecretParams{
			VersionID:                pgvalue.UUID(versionID),
			Version:                  version,
			Nonce:                    encrypted.nonce,
			Ciphertext:               encrypted.ciphertext,
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

type mutationReceipt struct {
	SecretID        string `json:"secretId"`
	SecretVersionID string `json:"secretVersionId"`
}

func (s *Store) Create(
	ctx context.Context,
	environmentID uuid.UUID,
	name string,
	value []byte,
	idempotencyKey string,
) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	if err := ValidateName(name); err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("begin secret creation: %w", err)
	}
	defer tx.Rollback(ctx)

	queries := db.New(tx)
	var claim *db.IdempotencyClaim
	if idempotencyKey != "" {
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		request, err := idempotency.NewSecretCreateRequest(
			environmentID,
			name,
			idempotencyKey,
		)
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		acquired, err := claims.Acquire(ctx, request)
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		if !acquired.New {
			snapshot, err := s.replayMutation(ctx, queries, acquired.Claim, value)
			if err != nil {
				return db.GetSecretSnapshotRow{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret creation replay: %w", err)
			}
			return snapshot, nil
		}
		claim = &acquired.Claim
	}

	bound := *s
	bound.db = queries
	record, err := bound.create(ctx, environmentID, name, value)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	snapshot, err := queries.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            record.ID,
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if claim != nil {
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		if err := completeMutation(ctx, claims, *claim, record.ID, record.CurrentVersionID); err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret creation: %w", err)
	}
	return snapshot, nil
}

func (s *Store) Rotate(
	ctx context.Context,
	environmentID uuid.UUID,
	secretID uuid.UUID,
	value []byte,
	idempotencyKey string,
) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("begin secret rotation: %w", err)
	}
	defer tx.Rollback(ctx)
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	queries := claims.Queries()
	request, err := idempotency.NewSecretRotateRequest(
		environmentID,
		secretID,
		strings.TrimSpace(idempotencyKey),
	)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	acquired, err := claims.Acquire(ctx, request)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if !acquired.New {
		snapshot, err := s.replayMutation(ctx, queries, acquired.Claim, value)
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret rotation replay: %w", err)
		}
		return snapshot, nil
	}
	bound := *s
	bound.db = queries
	rotated, err := bound.rotate(ctx, environmentID, secretID, value)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	snapshot, err := queries.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            rotated.ID,
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if err := completeMutation(ctx, claims, acquired.Claim, rotated.ID, rotated.CurrentVersionID); err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret rotation: %w", err)
	}
	return snapshot, nil
}

func completeMutation(
	ctx context.Context,
	claims *idempotency.Transaction,
	claim db.IdempotencyClaim,
	secretID pgtype.UUID,
	secretVersionID pgtype.UUID,
) error {
	receipt, err := json.Marshal(mutationReceipt{
		SecretID:        pgvalue.MustUUIDValue(secretID).String(),
		SecretVersionID: pgvalue.MustUUIDValue(secretVersionID).String(),
	})
	if err != nil {
		return fmt.Errorf("marshal secret mutation receipt: %w", err)
	}
	_, err = claims.Complete(ctx, claim, receipt)
	return err
}

func (s *Store) replayMutation(
	ctx context.Context,
	queries *db.Queries,
	claim db.IdempotencyClaim,
	value []byte,
) (db.GetSecretSnapshotRow, error) {
	if claim.State != "completed" {
		return db.GetSecretSnapshotRow{}, fmt.Errorf(
			"secret mutation claim is %s",
			claim.State,
		)
	}
	var receipt mutationReceipt
	if err := json.Unmarshal(claim.Receipt, &receipt); err != nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret mutation receipt is invalid")
	}
	secretID, err := ids.Parse(receipt.SecretID)
	if err != nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret mutation receipt is invalid")
	}
	secretVersionID, err := ids.Parse(receipt.SecretVersionID)
	if err != nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret mutation receipt is invalid")
	}
	version, err := queries.LockSecretVersion(ctx, db.LockSecretVersionParams{
		EnvironmentID: claim.EnvironmentID,
		SecretID:      pgvalue.UUID(secretID),
		VersionID:     pgvalue.UUID(secretVersionID),
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("lock replayed secret version: %w", err)
	}
	plaintext, err := s.decryptVersion(
		pgvalue.MustUUIDValue(claim.EnvironmentID),
		secretID,
		secretVersionID,
		version,
	)
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("decrypt replayed secret version: %w", err)
	}
	if subtle.ConstantTimeCompare(plaintext, value) != 1 {
		return db.GetSecretSnapshotRow{}, idempotency.ConflictError{
			ClaimID: pgvalue.MustUUIDValue(claim.ID),
		}
	}
	return queries.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{
		EnvironmentID: claim.EnvironmentID,
		ID:            pgvalue.UUID(secretID),
	})
}

func (s *Store) Revoke(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, idempotencyKey string) (db.GetSecretSnapshotRow, error) {
	if s.tx == nil {
		return db.GetSecretSnapshotRow{}, errors.New("secret transaction beginner is required")
	}
	request, err := idempotency.NewSecretRevokeRequest(
		environmentID,
		secretID,
		strings.TrimSpace(idempotencyKey),
	)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("begin secret revocation: %w", err)
	}
	defer tx.Rollback(ctx)
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	acquired, err := claims.Acquire(ctx, request)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	queries := claims.Queries()
	if !acquired.New {
		if acquired.Claim.State != "completed" {
			return db.GetSecretSnapshotRow{}, fmt.Errorf(
				"secret revoke claim is %s",
				acquired.Claim.State,
			)
		}
		snapshot, err := queries.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			ID:            pgvalue.UUID(secretID),
		})
		if err != nil {
			return db.GetSecretSnapshotRow{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret revoke replay: %w", err)
		}
		return snapshot, nil
	}
	bound := *s
	bound.db = queries
	record, changed, err := bound.revoke(ctx, environmentID, secretID)
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if changed {
		payload, err := json.Marshal(map[string]any{
			"environmentId":        environmentID.String(),
			"revocationGeneration": record.RevocationGeneration,
			"secretId":             secretID.String(),
		})
		if err != nil {
			return db.GetSecretSnapshotRow{}, fmt.Errorf("marshal secret revocation intent: %w", err)
		}
		if _, err := queries.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
			ID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Lane:         "control",
			Topic:        "secret.revoked",
			PartitionKey: secretID.String(),
			Payload:      payload,
			AvailableAt:  pgvalue.TimestamptzUTCZeroInvalid(time.Now()),
		}); err != nil {
			return db.GetSecretSnapshotRow{}, fmt.Errorf("create secret revocation intent: %w", err)
		}
	}
	snapshot, err := queries.GetSecretSnapshot(ctx, db.GetSecretSnapshotParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            pgvalue.UUID(secretID),
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	receipt, err := json.Marshal(map[string]any{
		"revocationGeneration": record.RevocationGeneration,
		"secretId":             secretID.String(),
		"stateVersion":         record.StateVersion,
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("marshal secret revoke receipt: %w", err)
	}
	if _, err := claims.Complete(ctx, acquired.Claim, receipt); err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GetSecretSnapshotRow{}, fmt.Errorf("commit secret revocation: %w", err)
	}
	return snapshot, nil
}

func (s *Store) revoke(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID) (db.Secret, bool, error) {
	var lastErr error
	for range maxWriteAttempts {
		record, err := s.db.GetSecret(ctx, db.GetSecretParams{
			EnvironmentID: pgvalue.UUID(environmentID),
			ID:            pgvalue.UUID(secretID),
		})
		if err != nil {
			return db.Secret{}, false, err
		}
		if record.State == "revoked" {
			return record, false, nil
		}
		if record.State != "active" {
			return db.Secret{}, false, UnavailableError{Err: fmt.Errorf("secret %q is %s", record.Name, record.State)}
		}
		revoked, err := s.db.RevokeSecret(ctx, db.RevokeSecretParams{
			EnvironmentID:        pgvalue.UUID(environmentID),
			ID:                   record.ID,
			ExpectedStateVersion: record.StateVersion,
		})
		if err == nil {
			return revoked, true, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			lastErr = err
			continue
		}
		return db.Secret{}, false, err
	}
	return db.Secret{}, false, fmt.Errorf("revoke secret after concurrent updates: %w", lastErr)
}

func (s *Store) Put(ctx context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return db.Secret{}, err
	}
	return s.PutScoped(ctx, orgID, projectID, environmentID, name, value)
}

func (s *Store) PutScoped(ctx context.Context, _ uuid.UUID, _ uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	if s.tx == nil {
		return db.Secret{}, errors.New("secret transaction beginner is required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.Secret{}, fmt.Errorf("begin secret mutation: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	bound := *s
	bound.db = queries
	record, err := bound.putScoped(ctx, environmentID, name, value)
	if err != nil {
		return db.Secret{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Secret{}, fmt.Errorf("commit secret mutation: %w", err)
	}
	return record, nil
}

func (s *Store) putScoped(ctx context.Context, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	record, err := s.db.GetSecretByName(ctx, db.GetSecretByNameParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.create(ctx, environmentID, name, value)
	case err != nil:
		return db.Secret{}, err
	case record.State != "active":
		return db.Secret{}, UnavailableError{Err: fmt.Errorf("secret %q is %s", name, record.State)}
	default:
		secretID, err := pgvalue.UUIDValue(record.ID)
		if err != nil {
			return db.Secret{}, err
		}
		return s.rotate(ctx, environmentID, secretID, value)
	}
}

func (s *Store) CheckNames(ctx context.Context, orgID uuid.UUID, names []string) error {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return err
	}
	return s.CheckScopedNames(ctx, orgID, projectID, environmentID, names)
}

func (s *Store) ResolveNames(ctx context.Context, orgID uuid.UUID, names []string) (Resolved, error) {
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
		secret, _, err := s.currentByName(ctx, environmentID, name)
		if err != nil {
			return fmt.Errorf("secret %q is unavailable: %w", name, err)
		}
		if secret.State != "active" {
			return UnavailableError{Err: fmt.Errorf("secret %q is %s", name, secret.State)}
		}
	}
	return nil
}

func (s *Store) ResolveScopedNames(ctx context.Context, _ uuid.UUID, _ uuid.UUID, environmentID uuid.UUID, names []string) (Resolved, error) {
	if len(names) == 0 {
		return Resolved{}, nil
	}
	resolved := make(Resolved, len(names))
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
