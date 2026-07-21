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
	"strings"

	"github.com/google/uuid"
)

const (
	FencingKeySize    = 32
	fencingDomain     = "helmr.workspace-fence.v0\x00"
	fingerprintDomain = "helmr.workspace-fence-key.v0\x00"
)

type FenceInput struct {
	LeaseID                uuid.UUID
	WorkspaceID            uuid.UUID
	OwnershipGeneration    int64
	WriterGeneration       int64
	MountFencingGeneration int64
}

type FencingCapability struct {
	KeyFingerprint FencingKeyFingerprint
	Token          string
	Hash           string
}

type FencingKeyFingerprint [sha256.Size]byte

type FencingKeys struct {
	active FencingKeyFingerprint
	keys   map[FencingKeyFingerprint][FencingKeySize]byte
}

func NewFencingKeys(active string, keys map[string][]byte) (FencingKeys, error) {
	activeFingerprint, err := ParseFencingKeyFingerprint(active)
	if err != nil {
		return FencingKeys{}, fmt.Errorf("active Workspace fencing key fingerprint: %w", err)
	}
	if len(keys) == 0 {
		return FencingKeys{}, errors.New("at least one Workspace fencing key is required")
	}
	copied := make(map[FencingKeyFingerprint][FencingKeySize]byte, len(keys))
	for encodedFingerprint, raw := range keys {
		fingerprint, err := ParseFencingKeyFingerprint(encodedFingerprint)
		if err != nil {
			return FencingKeys{}, fmt.Errorf(
				"Workspace fencing key fingerprint %q: %w",
				encodedFingerprint,
				err,
			)
		}
		if len(raw) != FencingKeySize {
			return FencingKeys{}, fmt.Errorf(
				"Workspace fencing key %q must be %d bytes, got %d",
				encodedFingerprint,
				FencingKeySize,
				len(raw),
			)
		}
		var key [FencingKeySize]byte
		copy(key[:], raw)
		if actual := FencingKeyFingerprintForKey(key); actual != fingerprint {
			return FencingKeys{}, fmt.Errorf(
				"Workspace fencing key %q does not match its fingerprint",
				encodedFingerprint,
			)
		}
		copied[fingerprint] = key
	}
	if _, ok := copied[activeFingerprint]; !ok {
		return FencingKeys{}, fmt.Errorf(
			"active Workspace fencing key %q is not configured",
			active,
		)
	}
	return FencingKeys{active: activeFingerprint, keys: copied}, nil
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
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key fingerprint: %w", err)
		}
		id, ok := token.(string)
		if !ok {
			return FencingKeys{}, errors.New("decode Workspace fencing key fingerprint: expected object key")
		}
		if _, exists := keys[id]; exists {
			return FencingKeys{}, fmt.Errorf("decode Workspace fencing key %q: duplicate fingerprint", id)
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

func (k FencingKeys) ActiveFingerprint() FencingKeyFingerprint {
	return k.active
}

func (k FencingKeys) Has(fingerprint FencingKeyFingerprint) bool {
	_, ok := k.keys[fingerprint]
	return ok
}

func (k FencingKeys) DeriveActive(input FenceInput) (FencingCapability, error) {
	return k.Derive(k.active, input)
}

func (k FencingKeys) Derive(
	fingerprint FencingKeyFingerprint,
	input FenceInput,
) (FencingCapability, error) {
	key, ok := k.keys[fingerprint]
	if !ok {
		return FencingCapability{}, fmt.Errorf(
			"Workspace fencing key %q is not configured",
			fingerprint,
		)
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
		KeyFingerprint: fingerprint,
		Token:          base64.RawURLEncoding.EncodeToString(raw),
		Hash:           "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

func ParseFencingKeyFingerprint(raw string) (FencingKeyFingerprint, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(raw, prefix) ||
		len(raw) != len(prefix)+sha256.Size*2 ||
		raw != strings.ToLower(raw) {
		return FencingKeyFingerprint{}, errors.New(
			"fingerprint must be lowercase sha256:<64 hexadecimal digits>",
		)
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return FencingKeyFingerprint{}, errors.New(
			"fingerprint must be lowercase sha256:<64 hexadecimal digits>",
		)
	}
	var fingerprint FencingKeyFingerprint
	copy(fingerprint[:], decoded)
	return fingerprint, nil
}

func FencingKeyFingerprintFromBytes(raw []byte) (FencingKeyFingerprint, error) {
	if len(raw) != sha256.Size {
		return FencingKeyFingerprint{}, fmt.Errorf(
			"Workspace fencing key fingerprint must be %d bytes, got %d",
			sha256.Size,
			len(raw),
		)
	}
	var fingerprint FencingKeyFingerprint
	copy(fingerprint[:], raw)
	return fingerprint, nil
}

func FencingKeyFingerprintForKey(key [FencingKeySize]byte) FencingKeyFingerprint {
	hash := sha256.New()
	_, _ = hash.Write([]byte(fingerprintDomain))
	_, _ = hash.Write(key[:])
	var fingerprint FencingKeyFingerprint
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint
}

func (f FencingKeyFingerprint) String() string {
	return "sha256:" + hex.EncodeToString(f[:])
}

func (f FencingKeyFingerprint) Bytes() []byte {
	return append([]byte(nil), f[:]...)
}
