package token

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

const (
	CredentialKeySize = 32

	credentialKeyDomain = "helmr.token-credential-key.v0\x00"
	callbackDomain      = "helmr.token-callback.v0\x00"
	bearerDomain        = "helmr.token-bearer.v0\x00"
)

type CredentialKeyID [sha256.Size]byte

type CredentialKeys struct {
	active CredentialKeyID
	keys   map[CredentialKeyID][CredentialKeySize]byte
}

type Credentials struct {
	KeyID               CredentialKeyID
	CallbackSecret      string
	CallbackFingerprint []byte
	PublicAccessToken   string
	PublicAccessHash    []byte
}

func NewCredentialKeys(active string, keys map[string][]byte) (CredentialKeys, error) {
	activeID, err := ParseCredentialKeyID(active)
	if err != nil {
		return CredentialKeys{}, fmt.Errorf("active Token credential key ID: %w", err)
	}
	if len(keys) == 0 {
		return CredentialKeys{}, errors.New("at least one Token credential key is required")
	}
	copied := make(map[CredentialKeyID][CredentialKeySize]byte, len(keys))
	for encodedID, raw := range keys {
		id, err := ParseCredentialKeyID(encodedID)
		if err != nil {
			return CredentialKeys{}, fmt.Errorf("Token credential key ID %q: %w", encodedID, err)
		}
		if len(raw) != CredentialKeySize {
			return CredentialKeys{}, fmt.Errorf(
				"Token credential key %q must be %d bytes, got %d",
				encodedID,
				CredentialKeySize,
				len(raw),
			)
		}
		var key [CredentialKeySize]byte
		copy(key[:], raw)
		if actual := CredentialKeyIDForKey(key); actual != id {
			return CredentialKeys{}, fmt.Errorf(
				"Token credential key %q does not match its ID",
				encodedID,
			)
		}
		copied[id] = key
	}
	if _, ok := copied[activeID]; !ok {
		return CredentialKeys{}, fmt.Errorf("active Token credential key %q is not configured", active)
	}
	return CredentialKeys{active: activeID, keys: copied}, nil
}

func CredentialKeysFromBase64JSON(active string, raw string) (CredentialKeys, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	token, err := decoder.Token()
	if err != nil {
		return CredentialKeys{}, fmt.Errorf("decode Token credential keys: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return CredentialKeys{}, errors.New("decode Token credential keys: expected JSON object")
	}
	keys := make(map[string][]byte)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return CredentialKeys{}, fmt.Errorf("decode Token credential key ID: %w", err)
		}
		id, ok := token.(string)
		if !ok {
			return CredentialKeys{}, errors.New("decode Token credential key ID: expected object key")
		}
		if _, exists := keys[id]; exists {
			return CredentialKeys{}, fmt.Errorf("decode Token credential key %q: duplicate ID", id)
		}
		var encoded string
		if err := decoder.Decode(&encoded); err != nil {
			return CredentialKeys{}, fmt.Errorf("decode Token credential key %q: %w", id, err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return CredentialKeys{}, fmt.Errorf("decode Token credential key %q: %w", id, err)
		}
		keys[id] = key
	}
	if _, err := decoder.Token(); err != nil {
		return CredentialKeys{}, fmt.Errorf("decode Token credential keys: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return CredentialKeys{}, errors.New("decode Token credential keys: trailing value")
	}
	return NewCredentialKeys(active, keys)
}

func (k CredentialKeys) ActiveID() CredentialKeyID {
	return k.active
}

func (k CredentialKeys) Has(id CredentialKeyID) bool {
	_, ok := k.keys[id]
	return ok
}

func (k CredentialKeys) DeriveActive(tokenID uuid.UUID) (Credentials, error) {
	return k.Derive(k.active, tokenID)
}

func (k CredentialKeys) Derive(id CredentialKeyID, tokenID uuid.UUID) (Credentials, error) {
	key, ok := k.keys[id]
	if !ok {
		return Credentials{}, fmt.Errorf("Token credential key %q is not configured", id)
	}
	if tokenID == uuid.Nil {
		return Credentials{}, errors.New("Token ID is required")
	}
	callback, err := deriveCredential(key, callbackDomain, tokenID)
	if err != nil {
		return Credentials{}, fmt.Errorf("derive Token callback credential: %w", err)
	}
	bearer, err := deriveCredential(key, bearerDomain, tokenID)
	if err != nil {
		return Credentials{}, fmt.Errorf("derive Token bearer credential: %w", err)
	}
	callbackSecret := base64.RawURLEncoding.EncodeToString(callback)
	publicAccessToken := "hlmr_pat_" + base64.RawURLEncoding.EncodeToString(bearer)
	callbackHash := sha256.Sum256([]byte(callbackSecret))
	bearerHash := sha256.Sum256([]byte(publicAccessToken))
	return Credentials{
		KeyID:               id,
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

func ParseCredentialKeyID(raw string) (CredentialKeyID, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(raw, prefix) ||
		len(raw) != len(prefix)+sha256.Size*2 ||
		raw != strings.ToLower(raw) {
		return CredentialKeyID{}, errors.New("ID must be lowercase sha256:<64 hexadecimal digits>")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return CredentialKeyID{}, errors.New("ID must be lowercase sha256:<64 hexadecimal digits>")
	}
	var id CredentialKeyID
	copy(id[:], decoded)
	return id, nil
}

func CredentialKeyIDFromBytes(raw []byte) (CredentialKeyID, error) {
	if len(raw) != sha256.Size {
		return CredentialKeyID{}, fmt.Errorf(
			"Token credential key ID must be %d bytes, got %d",
			sha256.Size,
			len(raw),
		)
	}
	var id CredentialKeyID
	copy(id[:], raw)
	return id, nil
}

func CredentialKeyIDForKey(key [CredentialKeySize]byte) CredentialKeyID {
	hash := sha256.New()
	_, _ = hash.Write([]byte(credentialKeyDomain))
	_, _ = hash.Write(key[:])
	var id CredentialKeyID
	copy(id[:], hash.Sum(nil))
	return id
}

func (id CredentialKeyID) String() string {
	return "sha256:" + hex.EncodeToString(id[:])
}

func deriveCredential(key [CredentialKeySize]byte, domain string, tokenID uuid.UUID) ([]byte, error) {
	return hkdf.Key(sha256.New, key[:], tokenID[:], domain+tokenID.String(), 32)
}
