// Package sha256sum formats SHA-256 digests consistently.
package sha256sum

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

const Prefix = "sha256:"

func ValidDigest(value string) bool {
	if len(value) != len(Prefix)+sha256.Size*2 || value[:len(Prefix)] != Prefix {
		return false
	}
	for _, character := range value[len(Prefix):] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func HexBytes(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

func DigestBytes(bytes []byte) string {
	return Prefix + HexBytes(bytes)
}

func HexHash(hash hash.Hash) string {
	return hex.EncodeToString(hash.Sum(nil))
}

func DigestHash(hash hash.Hash) string {
	return Prefix + HexHash(hash)
}
