// Package computerkey wraps Computer data keys for the Control Plane. Wrapping
// material and provider credentials must never be delivered to a Worker or guest.
package computerkey

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

const Size = 32
const MaxWrappedSize = 6144

type Envelope struct {
	WrappingKeyID string
	Ciphertext    []byte
}

func contextBytes(scope, keyID string) ([]byte, error) {
	if scope == "" || len(scope) > 256 || !utf8.ValidString(scope) || keyID == "" || len(keyID) > 128 || !utf8.ValidString(keyID) {
		return nil, errors.New("invalid computer key context")
	}
	return json.Marshal([3]string{"helmr.computer-key.v1", scope, keyID})
}

// Local is explicitly configured for self-hosted deployments, never selected as
// recovery from a managed provider failure. Its ID identifies operator-held material.
type Local struct {
	id   string
	aead cipher.AEAD
}

func NewLocal(id string, key []byte) (*Local, error) {
	if id == "" || len(id) > 2048 || len(key) != Size {
		return nil, errors.New("invalid local wrapping key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Local{id: id, aead: aead}, nil
}
func (l *Local) Wrap(ctx context.Context, scope, keyID string, plain []byte) (Envelope, error) {
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}
	aad, err := contextBytes(scope, keyID)
	if err != nil {
		return Envelope{}, err
	}
	if len(plain) != Size {
		return Envelope{}, errors.New("invalid computer data key")
	}
	nonce := make([]byte, l.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{WrappingKeyID: l.id, Ciphertext: l.aead.Seal(nonce, nonce, plain, aad)}, nil
}
func (l *Local) Unwrap(ctx context.Context, scope, keyID string, e Envelope) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aad, err := contextBytes(scope, keyID)
	if err != nil {
		return nil, err
	}
	n := l.aead.NonceSize()
	if e.WrappingKeyID != l.id || len(e.Ciphertext) != n+Size+l.aead.Overhead() {
		return nil, errors.New("invalid computer key envelope")
	}
	return l.aead.Open(nil, e.Ciphertext[:n], e.Ciphertext[n:], aad)
}
