package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCanonicalEmptyTreeDigest(t *testing.T) {
	sum := sha256.Sum256([]byte(TreeDigestDomain))
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != CanonicalEmptyTreeDigest {
		t.Fatalf("empty tree digest = %q, want %q", got, CanonicalEmptyTreeDigest)
	}
}
