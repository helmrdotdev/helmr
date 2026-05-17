package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

const DefaultKeyID = "helmr-managed:v1"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Store struct {
	db    db.Querier
	keyID string
	aead  cipher.AEAD
	rand  io.Reader
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

func New(database db.Querier, keyID string, key []byte) (*Store, error) {
	if database == nil {
		return nil, errors.New("secret database is required")
	}
	if strings.TrimSpace(keyID) == "" {
		keyID = DefaultKeyID
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("configure secret encryption key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("configure secret cipher: %w", err)
	}
	return &Store{db: database, keyID: keyID, aead: aead, rand: rand.Reader}, nil
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

func ValidateBindings(bindings api.SecretBindings) error {
	for declared, stored := range bindings {
		if err := ValidateName(declared); err != nil {
			return fmt.Errorf("invalid declared secret name: %w", err)
		}
		if _, err := storedNameFromBinding(stored); err != nil {
			return fmt.Errorf("invalid stored secret binding for %q: %w", declared, err)
		}
	}
	return nil
}

func storedNameFromBinding(binding string) (string, error) {
	scheme, value, ok := strings.Cut(binding, ":")
	if !ok {
		return "", errors.New("secret binding source must use vault:SECRET_NAME")
	}
	if scheme != "vault" {
		return "", fmt.Errorf("unsupported secret binding scheme %q", scheme)
	}
	if strings.ContainsAny(value, "/?") {
		return "", errors.New("vault secret name must not contain '/' or '?'")
	}
	if err := ValidateName(value); err != nil {
		return "", fmt.Errorf("invalid vault secret name: %w", err)
	}
	return value, nil
}

func (s *Store) Put(ctx context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return db.Secret{}, err
	}
	return s.PutScoped(ctx, orgID, projectID, environmentID, name, value)
}

func (s *Store) PutScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	if err := ValidateName(name); err != nil {
		return db.Secret{}, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return db.Secret{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext := s.aead.Seal(nil, nonce, value, scopedAdditionalData(orgID, projectID, environmentID, name, s.keyID))
	return s.db.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(orgID),
		ProjectID:     ids.ToPG(projectID),
		EnvironmentID: ids.ToPG(environmentID),
		Name:          name,
		KeyID:         s.keyID,
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	})
}

func (s *Store) Check(ctx context.Context, orgID uuid.UUID, bindings api.SecretBindings) error {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return err
	}
	return s.CheckScoped(ctx, orgID, projectID, environmentID, bindings)
}

func (s *Store) Resolve(ctx context.Context, orgID uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error) {
	projectID, environmentID, err := s.defaultScope(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.ResolveScoped(ctx, orgID, projectID, environmentID, bindings)
}

func (s *Store) CheckScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, bindings api.SecretBindings) error {
	if len(bindings) == 0 {
		return nil
	}
	if err := ValidateBindings(bindings); err != nil {
		return err
	}
	for declared, binding := range bindings {
		stored, _, err := s.scopedSecretBinding(ctx, orgID, projectID, environmentID, binding)
		if err != nil {
			return fmt.Errorf("secret binding %q references unavailable secret %q: %w", declared, stored, err)
		}
	}
	return nil
}

func (s *Store) ResolveScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error) {
	if len(bindings) == 0 {
		return api.ResolvedSecrets{}, nil
	}
	if err := ValidateBindings(bindings); err != nil {
		return nil, UnavailableError{Err: err}
	}
	resolved := make(api.ResolvedSecrets, len(bindings))
	for declared, binding := range bindings {
		stored, record, err := s.scopedSecretBinding(ctx, orgID, projectID, environmentID, binding)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, UnavailableError{Err: fmt.Errorf("resolve secret %q from %q: %w", declared, stored, err)}
			}
			return nil, fmt.Errorf("resolve secret %q from %q: %w", declared, stored, err)
		}
		if record.KeyID != s.keyID {
			return nil, UnavailableError{Err: fmt.Errorf("secret %q uses unsupported key id %q", stored, record.KeyID)}
		}
		plaintext, err := s.aead.Open(nil, record.Nonce, record.Ciphertext, scopedAdditionalData(orgID, projectID, environmentID, record.Name, record.KeyID))
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("decrypt secret %q: %w", stored, err)}
		}
		resolved[declared] = plaintext
	}
	return resolved, nil
}

func (s *Store) scopedSecretBinding(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, binding string) (string, db.Secret, error) {
	stored, err := storedNameFromBinding(binding)
	if err != nil {
		return "", db.Secret{}, err
	}
	record, err := s.db.GetScopedSecretByName(ctx, db.GetScopedSecretByNameParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     ids.ToPG(projectID),
		EnvironmentID: ids.ToPG(environmentID),
		Name:          stored,
	})
	if err != nil {
		return stored, db.Secret{}, err
	}
	return stored, record, nil
}

func scopedAdditionalData(orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, keyID string) []byte {
	return []byte(orgID.String() + "\x00" + projectID.String() + "\x00" + environmentID.String() + "\x00" + name + "\x00" + keyID)
}

func (s *Store) defaultScope(ctx context.Context, orgID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	scope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	projectID, err := ids.FromPG(scope.ProjectID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	environmentID, err := ids.FromPG(scope.EnvironmentID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return projectID, environmentID, nil
}
