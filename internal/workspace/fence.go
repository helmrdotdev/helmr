package workspace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	FencingKeySize    = 32
	fencingDomain     = "helmr.workspace-fence.v0\x00"
	fingerprintDomain = "helmr.workspace-fence-key.v0\x00"
)

var fencingKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type FenceInput struct {
	LeaseID                uuid.UUID
	WorkspaceID            uuid.UUID
	OwnershipGeneration    int64
	WriterGeneration       int64
	MountFencingGeneration int64
}

type FencingCapability struct {
	KeyID string
	Token string
	Hash  string
}

type FencingKeys struct {
	active string
	keys   map[string][FencingKeySize]byte
}

func NewFencingKeys(active string, keys map[string][]byte) (FencingKeys, error) {
	if !fencingKeyIDPattern.MatchString(active) {
		return FencingKeys{}, errors.New("active Workspace fencing key ID is invalid")
	}
	if len(keys) == 0 {
		return FencingKeys{}, errors.New("at least one Workspace fencing key is required")
	}
	copied := make(map[string][FencingKeySize]byte, len(keys))
	for id, raw := range keys {
		if !fencingKeyIDPattern.MatchString(id) {
			return FencingKeys{}, fmt.Errorf("Workspace fencing key ID %q is invalid", id)
		}
		if len(raw) != FencingKeySize {
			return FencingKeys{}, fmt.Errorf("Workspace fencing key %q must be %d bytes, got %d", id, FencingKeySize, len(raw))
		}
		var key [FencingKeySize]byte
		copy(key[:], raw)
		copied[id] = key
	}
	if _, ok := copied[active]; !ok {
		return FencingKeys{}, fmt.Errorf("active Workspace fencing key %q is not configured", active)
	}
	return FencingKeys{active: active, keys: copied}, nil
}

func FencingKeysFromBase64JSON(active string, raw string) (FencingKeys, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	token, err := decoder.Token()
	if err != nil {
		return FencingKeys{}, fmt.Errorf("decode Workspace fencing keys: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return FencingKeys{}, errors.New("decode Workspace fencing keys: expected JSON object")
	}
	keys := make(map[string][]byte)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key ID: %w", err)
		}
		id, ok := token.(string)
		if !ok {
			return FencingKeys{}, errors.New("decode Workspace fencing key ID: expected object key")
		}
		if _, exists := keys[id]; exists {
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key %q: duplicate ID", id)
		}
		var encoded string
		if err := decoder.Decode(&encoded); err != nil {
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key %q: %w", id, err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key %q: %w", id, err)
		}
		keys[id] = key
	}
	if _, err := decoder.Token(); err != nil {
		return FencingKeys{}, fmt.Errorf("decode Workspace fencing keys: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return FencingKeys{}, errors.New("decode Workspace fencing keys: trailing value")
	}
	return NewFencingKeys(active, keys)
}

func (k FencingKeys) ActiveID() string {
	return k.active
}

func (k FencingKeys) Has(id string) bool {
	_, ok := k.keys[id]
	return ok
}

func (k FencingKeys) DeriveActive(input FenceInput) (FencingCapability, error) {
	return k.Derive(k.active, input)
}

func (k FencingKeys) Derive(keyID string, input FenceInput) (FencingCapability, error) {
	key, ok := k.keys[keyID]
	if !ok {
		return FencingCapability{}, fmt.Errorf("Workspace fencing key %q is not configured", keyID)
	}
	if input.LeaseID == uuid.Nil {
		return FencingCapability{}, errors.New("Workspace Lease ID is required")
	}
	if input.WorkspaceID == uuid.Nil {
		return FencingCapability{}, errors.New("Workspace ID is required")
	}
	if input.OwnershipGeneration <= 0 || input.WriterGeneration <= 0 || input.MountFencingGeneration <= 0 {
		return FencingCapability{}, errors.New("Workspace fencing generations must be positive")
	}

	message := make([]byte, 0, len(fencingDomain)+32+24)
	message = append(message, fencingDomain...)
	message = append(message, input.LeaseID[:]...)
	message = append(message, input.WorkspaceID[:]...)
	message = binary.BigEndian.AppendUint64(message, uint64(input.OwnershipGeneration))
	message = binary.BigEndian.AppendUint64(message, uint64(input.WriterGeneration))
	message = binary.BigEndian.AppendUint64(message, uint64(input.MountFencingGeneration))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(message)
	raw := mac.Sum(nil)
	sum := sha256.Sum256(raw)
	return FencingCapability{
		KeyID: keyID,
		Token: base64.RawURLEncoding.EncodeToString(raw),
		Hash:  "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

func (k FencingKeys) Fingerprint(keyID string) ([sha256.Size]byte, error) {
	key, ok := k.keys[keyID]
	if !ok {
		return [sha256.Size]byte{}, fmt.Errorf("Workspace fencing key %q is not configured", keyID)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(fingerprintDomain))
	_, _ = hash.Write(key[:])
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}
