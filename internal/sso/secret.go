package sso

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"uuid"
)

var errSecretUnavailable = errors.New("the identity provider client secret is unavailable")

// sealer encrypts OIDC client secrets at rest. The authenticated data binds a
// ciphertext to its organization and connection, so a row copied elsewhere
// cannot be opened.
type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (sealer, error) {
	if len(key) != 32 {
		return sealer{}, errors.New("a 32-byte encryption key is required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return sealer{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealer{}, err
	}
	return sealer{aead: aead}, nil
}

func secretAAD(org, connection uuid.UUID) []byte {
	aad := []byte("helmr.sso-oidc-client-secret.v1\x00")
	aad = append(aad, org[:]...)
	return append(aad, connection[:]...)
}

func (s sealer) seal(org, connection uuid.UUID, secret string) (ciphertext, nonce []byte, err error) {
	if secret == "" {
		return nil, nil, errSecretUnavailable
	}
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, errSecretUnavailable
	}
	return s.aead.Seal(nil, nonce, []byte(secret), secretAAD(org, connection)), nonce, nil
}

func (s sealer) open(org, connection uuid.UUID, ciphertext, nonce []byte) (string, error) {
	if len(nonce) != s.aead.NonceSize() {
		return "", errSecretUnavailable
	}
	plain, err := s.aead.Open(nil, nonce, ciphertext, secretAAD(org, connection))
	if err != nil || len(plain) == 0 {
		return "", errSecretUnavailable
	}
	return string(plain), nil
}
