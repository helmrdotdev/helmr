package computer

import (
	"math"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestDiskArtifactMetadataBoundary(t *testing.T) {
	const capacity = int64(16 << 20)
	valid := DiskArtifact{
		Object:       cas.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, MediaType: DiskMediaType},
		LogicalBytes: capacity,
	}
	if err := valid.Validate(capacity); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		change   func(*DiskArtifact)
		capacity int64
	}{
		{"wrong-capacity", func(a *DiskArtifact) { a.LogicalBytes *= 2 }, capacity},
		{"wrong-format", func(a *DiskArtifact) { a.Object.MediaType = "application/octet-stream" }, capacity},
		{"invalid-digest", func(a *DiskArtifact) { a.Object.Digest = strings.ToUpper(a.Object.Digest) }, capacity},
		{"empty-object", func(a *DiskArtifact) { a.Object.SizeBytes = 0 }, capacity},
		{"oversized-object", func(a *DiskArtifact) { a.Object.SizeBytes = 3 * capacity }, capacity},
		{"unaligned-capacity", func(a *DiskArtifact) { a.LogicalBytes = capacity + 1 }, capacity + 1},
		{"zero-capacity", func(a *DiskArtifact) { a.LogicalBytes = 0 }, 0},
		{"overflow-capacity", func(a *DiskArtifact) { a.LogicalBytes = math.MaxInt64 &^ 4095 }, math.MaxInt64 &^ 4095},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.change(&candidate)
			if err := candidate.Validate(test.capacity); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	// The framing bound permits one extra read byte without signed overflow.
	const largestCapacity = ((math.MaxInt64 - (2 << 20) - 1) / 2) &^ 4095
	limit, err := diskArtifactLimit(largestCapacity)
	if err != nil || limit <= largestCapacity || limit == math.MaxInt64 {
		t.Fatalf("largest capacity bound = %d, %v", limit, err)
	}
	candidate := valid
	candidate.LogicalBytes = largestCapacity
	candidate.Object.SizeBytes = limit
	if err := candidate.Validate(largestCapacity); err != nil {
		t.Fatal(err)
	}
	candidate.Object.SizeBytes++
	if err := candidate.Validate(largestCapacity); err == nil {
		t.Fatal("artifact beyond framing bound accepted")
	}
}
