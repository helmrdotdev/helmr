// Package definitiontest holds test-only helpers for Deployment definition
// contracts, such as the canonical manifest digest that golden fixtures compare
// against.
package definitiontest

import (
	"crypto/sha256"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const manifestDigestDomain = "helmr.deployment-definition-manifest.v0\x00"

// CanonicalManifestAndDigest canonicalizes a definition manifest and returns its
// domain-separated digest.
func CanonicalManifestAndDigest(raw []byte) ([]byte, [sha256.Size]byte, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("canonicalize deployment manifest: %w", err)
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return nil, [sha256.Size]byte{}, fmt.Errorf("deployment manifest root must be an object")
	}
	hash := sha256.New()
	hash.Write([]byte(manifestDigestDomain))
	hash.Write(canonical)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return canonical, digest, nil
}
