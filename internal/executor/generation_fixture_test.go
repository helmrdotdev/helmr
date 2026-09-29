package executor

import (
	"github.com/helmrdotdev/helmr/internal/disk"
	"strings"
)

// Framing-only identity for tests that do not publish or read physical bytes.
func testGenerationRoot(capacity int64) disk.GenerationRoot {
	return disk.GenerationRoot{FormatVersion: 1, LogicalBytes: capacity,
		Pack: disk.GenerationPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2},
		Page: disk.GenerationPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8}
}

func ptrGenerationRoot(capacity int64) *disk.GenerationRoot {
	r := testGenerationRoot(capacity)
	return &r
}
