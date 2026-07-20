package keyedhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
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
		versions = append(versions, int(version))
	}
	sort.Sort(sort.Reverse(sort.IntSlice(versions)))
	digests := make([]Digest, 0, len(versions))
	for _, version := range versions {
		key := k.keys[int32(version)]
		mac := hmac.New(sha256.New, key[:])
		_, _ = mac.Write(value)
		var sum [sha256.Size]byte
		copy(sum[:], mac.Sum(nil))
		digests = append(digests, Digest{Version: int32(version), Value: sum})
	}
	return digests
}
