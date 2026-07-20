package keyedhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const KeySize = 32

type Keyring struct {
	keys map[int32][KeySize]byte
}

type Digest struct {
	Version int32
	Value   [sha256.Size]byte
}

type Version struct {
	Number      int32
	Fingerprint []byte
	Current     bool
}

type Selection struct {
	Versions []int32
	Current  int32
}

func New(keys map[int32][]byte) (Keyring, error) {
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
	return Keyring{keys: copied}, nil
}

func FromBase64JSON(raw string) (Keyring, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	token, err := decoder.Token()
	if err != nil {
		return Keyring{}, fmt.Errorf("decode keyed-hash keys: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return Keyring{}, errors.New("decode keyed-hash keys: expected JSON object")
	}
	keys := make(map[int32][]byte)
	rawVersions := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Keyring{}, fmt.Errorf("decode keyed-hash version: %w", err)
		}
		rawVersion, ok := token.(string)
		if !ok {
			return Keyring{}, errors.New("decode keyed-hash version: expected object key")
		}
		if _, exists := rawVersions[rawVersion]; exists {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %q: duplicate key", rawVersion)
		}
		rawVersions[rawVersion] = struct{}{}
		version, err := strconv.ParseInt(rawVersion, 10, 32)
		if err != nil || version <= 0 || strconv.FormatInt(version, 10) != rawVersion {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %q", rawVersion)
		}
		if _, exists := keys[int32(version)]; exists {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %q: duplicate version", rawVersion)
		}
		var rawKey string
		if err := decoder.Decode(&rawKey); err != nil {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %d: %w", version, err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rawKey))
		if err != nil {
			return Keyring{}, fmt.Errorf("decode keyed-hash version %d: %w", version, err)
		}
		keys[int32(version)] = key
	}
	if _, err := decoder.Token(); err != nil {
		return Keyring{}, fmt.Errorf("decode keyed-hash keys: %w", err)
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return Keyring{}, fmt.Errorf("decode keyed-hash keys: %w", err)
		}
		return Keyring{}, fmt.Errorf("decode keyed-hash keys: unexpected token %v", token)
	}
	return New(keys)
}

func (k Keyring) Has(version int32) bool {
	_, ok := k.keys[version]
	return ok
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

func (k Keyring) Fingerprint(version int32) ([sha256.Size]byte, error) {
	key, ok := k.keys[version]
	if !ok {
		return [sha256.Size]byte{}, fmt.Errorf("keyed-hash version %d is not configured", version)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("helmr.lookup-hmac-key.v0"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(key[:])
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}

func (k Keyring) Select(versions []Version) (Selection, error) {
	if len(versions) == 0 {
		return Selection{}, errors.New("lookup HMAC authority has no active versions")
	}
	sorted := append([]Version(nil), versions...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Number < sorted[j].Number
	})
	selection := Selection{Versions: make([]int32, 0, len(sorted))}
	for index, version := range sorted {
		if version.Number <= 0 {
			return Selection{}, fmt.Errorf("lookup HMAC authority version %d must be positive", version.Number)
		}
		if index > 0 && sorted[index-1].Number == version.Number {
			return Selection{}, fmt.Errorf("lookup HMAC authority repeats version %d", version.Number)
		}
		expected, err := k.Fingerprint(version.Number)
		if err != nil {
			return Selection{}, err
		}
		if !hmac.Equal(expected[:], version.Fingerprint) {
			return Selection{}, fmt.Errorf("lookup HMAC version %d has different key bytes", version.Number)
		}
		selection.Versions = append(selection.Versions, version.Number)
		if version.Current {
			if selection.Current != 0 {
				return Selection{}, errors.New("lookup HMAC authority has multiple current versions")
			}
			selection.Current = version.Number
		}
	}
	if selection.Current == 0 {
		return Selection{}, errors.New("lookup HMAC authority has no current version")
	}
	return selection, nil
}
