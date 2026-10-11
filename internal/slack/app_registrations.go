package slack

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// AppCredentials is a write-only management input. Its contents must never be
// returned by a management read or copied into an Agent's environment.
type AppCredentials struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	SigningSecret string `json:"signing_secret"`
}

func (v AppCredentials) valid() bool {
	for _, value := range []string{v.ClientID, v.ClientSecret, v.SigningSecret} {
		if value == "" || len(value) > 4096 || strings.TrimSpace(value) != value {
			return false
		}
	}
	return true
}

func appCredentialAAD(org, registration uuid.UUID, revision int64) []byte {
	aad := []byte("helmr.slack-app-credentials.v1\x00")
	aad = append(aad, org[:]...)
	aad = append(aad, registration[:]...)
	return binary.BigEndian.AppendUint64(aad, uint64(revision))
}

func (s *CredentialStore) sealApp(org, registration uuid.UUID, revision int64, credentials AppCredentials) ([]byte, []byte, error) {
	if revision < 1 || !credentials.valid() {
		return nil, nil, errCredentialUnavailable
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		return nil, nil, errCredentialUnavailable
	}
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, errCredentialUnavailable
	}
	return s.encryption.Seal(nil, nonce, plaintext, appCredentialAAD(org, registration, revision)), nonce, nil
}

func (s *CredentialStore) openApp(org, registration uuid.UUID, revision int64, ciphertext, nonce []byte) (AppCredentials, error) {
	var credentials AppCredentials
	if revision < 1 || len(nonce) != s.encryption.NonceSize() {
		return credentials, errCredentialUnavailable
	}
	plaintext, err := s.encryption.Open(nil, nonce, ciphertext, appCredentialAAD(org, registration, revision))
	if err != nil || json.Unmarshal(plaintext, &credentials) != nil || !credentials.valid() {
		return AppCredentials{}, errCredentialUnavailable
	}
	return credentials, nil
}

// StoreAppCredentials rotates secrets for one registration. Once enrolled, the
// OAuth client is immutable: changing app identity requires new setup.
func (s *CredentialStore) StoreAppCredentials(ctx context.Context, org, user, registration uuid.UUID, credentials AppCredentials) error {
	if !credentials.valid() {
		return errCredentialUnavailable
	}
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var client *string
		var revision int64
		err := tx.QueryRow(ctx, `SELECT client_id,credential_revision FROM slack_app_registrations WHERE id=$1 AND organization_id=$2 AND retired_at IS NULL FOR NO KEY UPDATE`, registration, org).Scan(&client, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInstallationAuthority
		}
		if err != nil {
			return err
		}
		if client != nil && *client != credentials.ClientID {
			return errInstallationAuthority
		}
		revision++
		ciphertext, nonce, err := s.sealApp(org, registration, revision, credentials)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_app_registrations SET client_id=$2,credential_revision=$3,credential_ciphertext=$4,credential_nonce=$5 WHERE id=$1`, registration, credentials.ClientID, revision, ciphertext, nonce)
		return err
	})
}

// appCredentials is adapter-private and resolves exactly the selected app. A
// registration identifier chooses an expected secret; it is not authentication.
func (s *CredentialStore) appCredentials(ctx context.Context, registration uuid.UUID) (AppCredentials, error) {
	var credentials AppCredentials
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		var org uuid.UUID
		var revision int64
		var ciphertext, nonce []byte
		err := tx.QueryRow(ctx, `SELECT organization_id,credential_revision,credential_ciphertext,credential_nonce FROM slack_app_registrations WHERE id=$1 AND retired_at IS NULL FOR SHARE`, registration).Scan(&org, &revision, &ciphertext, &nonce)
		if err != nil {
			return errCredentialUnavailable
		}
		credentials, err = s.openApp(org, registration, revision, ciphertext, nonce)
		return err
	})
	if err != nil {
		return AppCredentials{}, errCredentialUnavailable
	}
	return credentials, nil
}

// RegistrationOAuth selects a dedicated OAuth client for installation or OpenID.
// It returns no secret-bearing configuration to the HTTP caller.
func (s *CredentialStore) RegistrationOAuth(ctx context.Context, registration uuid.UUID, transport http.RoundTripper) (*OAuthClient, error) {
	credentials, err := s.appCredentials(ctx, registration)
	if err != nil {
		return nil, err
	}
	return NewOAuthClient(credentials.ClientID, credentials.ClientSecret, transport)
}

func (s *CredentialStore) callbackCredentials(ctx context.Context, registration uuid.UUID) (string, []byte, error) {
	var appID string
	var secret []byte
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		var org uuid.UUID
		var app *string
		var revision int64
		var ciphertext, nonce []byte
		err := tx.QueryRow(ctx, `SELECT organization_id,app_id,credential_revision,credential_ciphertext,credential_nonce FROM slack_app_registrations WHERE id=$1 AND retired_at IS NULL FOR SHARE`, registration).Scan(&org, &app, &revision, &ciphertext, &nonce)
		if err != nil {
			return errCredentialUnavailable
		}
		credentials, err := s.openApp(org, registration, revision, ciphertext, nonce)
		if err != nil {
			return err
		}
		if app != nil {
			appID = *app
		}
		secret = []byte(credentials.SigningSecret)
		return nil
	})
	return appID, secret, err
}

// CallbackHandler uses only the registration selected by the endpoint. Payload
// app IDs cannot select a signing secret, and unknown IDs never trigger a scan.
func (s *CredentialStore) CallbackHandler(registration uuid.UUID, events EventHandler, interactions *InteractionHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, secret, err := s.callbackCredentials(r.Context(), registration)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if interactions != nil {
			handler := *interactions
			handler.AppID = app
			handler.SigningSecret = secret
			handler.ServeHTTP(w, r)
			return
		}
		handler := events
		handler.AppID = app
		handler.SigningSecret = secret
		handler.pendingRegistration = app == ""
		handler.ServeHTTP(w, r)
	})
}

func (s *CredentialStore) InstallationOAuth(ctx context.Context, installation uuid.UUID, transport http.RoundTripper) (uuid.UUID, *OAuthClient, error) {
	var registration uuid.UUID
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT app_registration_id FROM slack_installations WHERE id=$1 AND disconnected_at IS NULL AND authorization_lost_at IS NULL`, installation).Scan(&registration)
	})
	if err != nil {
		return uuid.Nil(), nil, errCredentialUnavailable
	}
	oauth, err := s.RegistrationOAuth(ctx, registration, transport)
	return registration, oauth, err
}
