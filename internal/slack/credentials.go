package slack

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var errCredentialUnavailable = errors.New("the Slack credential is unavailable")

// credentialBundle never crosses the adapter boundary. Expiry is authenticated
// inside the envelope as well as indexed in the installation for scheduling.
type credentialBundle struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

func (b credentialBundle) valid() bool {
	return b.AccessToken != "" && (b.RefreshToken != "") == (b.ExpiresAt != nil) && (b.ExpiresAt == nil || !b.ExpiresAt.IsZero())
}

type CredentialStore struct {
	pool       db.TxBeginner
	encryption cipher.AEAD
}

func NewCredentialStore(pool db.TxBeginner, key []byte) (*CredentialStore, error) {
	if pool == nil || len(key) != 32 {
		return nil, errors.New("the Slack credential database and 32-byte encryption key are required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	encryption, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &CredentialStore{pool: pool, encryption: encryption}, nil
}

func credentialAAD(org, installation uuid.UUID, revision int64) []byte {
	aad := []byte("helmr.slack-credentials.v1\x00")
	aad = append(aad, org[:]...)
	aad = append(aad, installation[:]...)
	return binary.BigEndian.AppendUint64(aad, uint64(revision))
}

func (s *CredentialStore) seal(org, installation uuid.UUID, revision int64, bundle credentialBundle) ([]byte, []byte, error) {
	if revision < 1 || !bundle.valid() {
		return nil, nil, errCredentialUnavailable
	}
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return nil, nil, errCredentialUnavailable
	}
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, errCredentialUnavailable
	}
	return s.encryption.Seal(nil, nonce, plaintext, credentialAAD(org, installation, revision)), nonce, nil
}

func (s *CredentialStore) open(org, installation uuid.UUID, revision int64, ciphertext, nonce []byte) (credentialBundle, error) {
	var bundle credentialBundle
	if revision < 1 || len(nonce) != s.encryption.NonceSize() {
		return bundle, errCredentialUnavailable
	}
	plaintext, err := s.encryption.Open(nil, nonce, ciphertext, credentialAAD(org, installation, revision))
	if err != nil {
		return bundle, errCredentialUnavailable
	}
	if json.Unmarshal(plaintext, &bundle) != nil || !bundle.valid() {
		return credentialBundle{}, errCredentialUnavailable
	}
	return bundle, nil
}

// BotToken reads the exact revision claimed by a caller. A refresh or revision
// change before HTTP is proven unsent, allowing frozen content to wait and retry.
func (s *CredentialStore) BotToken(ctx context.Context, installation uuid.UUID, revision int64) (string, error) {
	var token string
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		var org uuid.UUID
		var ciphertext, nonce []byte
		err := tx.QueryRow(ctx, `SELECT organization_id,credential_ciphertext,credential_nonce FROM slack_installations
 WHERE id=$1 AND credential_revision=$2 AND disconnected_at IS NULL AND authorization_lost_at IS NULL
 AND refresh_attempt_id IS NULL AND (credential_expires_at IS NULL OR credential_expires_at>clock_timestamp()) FOR SHARE`, installation, revision).Scan(&org, &ciphertext, &nonce)
		if err != nil {
			return errCredentialUnavailable
		}
		bundle, err := s.open(org, installation, revision, ciphertext, nonce)
		if err != nil {
			return err
		}
		if bundle.ExpiresAt != nil && !bundle.ExpiresAt.After(time.Now()) {
			return errCredentialUnavailable
		}
		token = bundle.AccessToken
		return nil
	})
	if err != nil {
		return "", errCredentialUnavailable
	}
	return token, nil
}
