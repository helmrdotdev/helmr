package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/release"
)

// A file whose digest or size differs from the signed index never reaches
// the store: publishStaged refuses it before publishing, here with no store.
func TestPublishStagedRefusesBytesTheIndexDoesNotList(t *testing.T) {
	data := []byte("released bytes")
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	for name, signed := range map[string]release.Asset{
		"other digest": {Digest: "sha256:" + strings.Repeat("0", 64), SizeBytes: int64(len(data))},
		"wrong size":   {Digest: digest, SizeBytes: int64(len(data)) + 1},
		"shorter size": {Digest: digest, SizeBytes: int64(len(data)) - 1},
	} {
		t.Run(name, func(t *testing.T) {
			err := publishStaged(t.Context(), nil, t.TempDir(), "asset", bytes.NewReader(data), signed)
			if err == nil || !strings.Contains(err.Error(), "signed index lists") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
