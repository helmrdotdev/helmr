package auth

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const CredentialKeySize = 32

const (
	callbackDomain = "helmr.token-callback.v0\x00"
	bearerDomain   = "helmr.token-bearer.v0\x00"
)

type CredentialKey struct {
	value []byte
}

type Credentials struct {
	CallbackSecret      string
	CallbackFingerprint []byte
	PublicAccessToken   string
	PublicAccessHash    []byte
}

func NewCredentialKey(raw []byte) (CredentialKey, error) {
	if len(raw) != CredentialKeySize {
		return CredentialKey{}, fmt.Errorf(
			"token credential key must be %d bytes, got %d",
			CredentialKeySize,
			len(raw),
		)
	}
	return CredentialKey{value: append([]byte(nil), raw...)}, nil
}

func (k CredentialKey) Valid() bool {
	return len(k.value) == CredentialKeySize
}

func (k CredentialKey) Derive(tokenID uuid.UUID) (Credentials, error) {
	if tokenID == uuid.Nil {
		return Credentials{}, errors.New("token ID is required")
	}
	if !k.Valid() {
		return Credentials{}, errors.New("token credential key is invalid")
	}
	callback, err := deriveCredential(k.value, callbackDomain, tokenID)
	if err != nil {
		return Credentials{}, fmt.Errorf("derive token callback credential: %w", err)
	}
	bearer, err := deriveCredential(k.value, bearerDomain, tokenID)
	if err != nil {
		return Credentials{}, fmt.Errorf("derive token bearer credential: %w", err)
	}
	callbackSecret := base64.RawURLEncoding.EncodeToString(callback)
	publicAccessToken := "hlmr_pat_" + base64.RawURLEncoding.EncodeToString(bearer)
	callbackHash := sha256.Sum256([]byte(callbackSecret))
	bearerHash := sha256.Sum256([]byte(publicAccessToken))
	return Credentials{
		CallbackSecret:      callbackSecret,
		CallbackFingerprint: append([]byte(nil), callbackHash[:]...),
		PublicAccessToken:   publicAccessToken,
		PublicAccessHash:    append([]byte(nil), bearerHash[:]...),
	}, nil
}

func HashCredential(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return append([]byte(nil), sum[:]...)
}

func deriveCredential(key []byte, domain string, tokenID uuid.UUID) ([]byte, error) {
	return hkdf.Key(sha256.New, key, tokenID[:], domain+tokenID.String(), 32)
}
