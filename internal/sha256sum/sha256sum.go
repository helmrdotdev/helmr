// Package sha256sum formats SHA-256 digests consistently.
package sha256sum

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

const Prefix = "sha256:"

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
