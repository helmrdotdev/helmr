package keyedhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const KeySize = 32

type Keyring struct {
	current int32
	keys    map[int32][KeySize]byte
}

type Digest struct {
	Version int32
	Value   [sha256.Size]byte
}

func New(current int32, keys map[int32][]byte) (Keyring, error) {
	if current <= 0 {
		return Keyring{}, errors.New("current keyed-hash version must be positive")
	}
	if len(keys) == 0 {
		return Keyring{}, errors.New("at least one keyed-hash key is required")
	}
	copied := make(map[int32][KeySize]byte, len(keys))
	for version, raw := range keys {
		if version <= 0 {
			return Keyring{}, fmt.Errorf("keyed-hash version %d must be positive", version)
		}
		if len(raw) != KeySize {
			return Keyring{}, fmt.Errorf("keyed-hash version %d must be %d bytes, got %d", version, KeySize, len(raw))
		}
		var key [KeySize]byte
		copy(key[:], raw)
		copied[version] = key
	}
	if _, ok := copied[current]; !ok {
		return Keyring{}, fmt.Errorf("current keyed-hash version %d is not configured", current)
	}
	return Keyring{current: current, keys: copied}, nil
}

func FromBase64JSON(raw string) (Keyring, error) {
	var registry struct {
		Current int32             `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &registry); err != nil {
		return Keyring{}, fmt.Errorf("decode keyed-hash keys: %w", err)
	}
	keys := make(map[int32][]byte, len(registry.Keys))
	for rawVersion, rawKey := range registry.Keys {
		version, err := strconv.ParseInt(rawVersion, 10, 32)
		if err != nil || version <= 0 {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %q", rawVersion)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rawKey))
		if err != nil {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %d: %w", version, err)
		}
		keys[int32(version)] = key
	}
	return New(registry.Current, keys)
}

func (k Keyring) CurrentVersion() int32 {
	return k.current
}

func (k Keyring) Sum(version int32, value []byte) (Digest, error) {
	key, ok := k.keys[version]
	if !ok {
		return Digest{}, fmt.Errorf("keyed-hash version %d is not configured", version)
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(value)
	var sum [sha256.Size]byte
	copy(sum[:], mac.Sum(nil))
	return Digest{Version: version, Value: sum}, nil
}

func (k Keyring) Current(value []byte) (Digest, error) {
	return k.Sum(k.current, value)
}

func (k Keyring) All(value []byte) []Digest {
	versions := make([]int, 0, len(k.keys))
	for version := range k.keys {
		if version == k.current {
			continue
		}
		versions = append(versions, int(version))
	}
	sort.Sort(sort.Reverse(sort.IntSlice(versions)))
	digests := make([]Digest, 0, len(versions)+1)
	current, _ := k.Sum(k.current, value)
	digests = append(digests, current)
	for _, version := range versions {
		digest, _ := k.Sum(int32(version), value)
		digests = append(digests, digest)
	}
	return digests
}
