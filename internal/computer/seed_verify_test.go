package computer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/klauspost/compress/zstd"
)

// The format is produced explicitly so malformed wire input is tested independently
// of the encoder and on every Control Plane development platform.
func TestVerifySeedWithoutDisk(t *testing.T) {
	const size = int64(32 << 30)
	var encoded bytes.Buffer
	encoded.WriteString("helmr-firecracker-filepack-v0\n")
	header := []byte(fmt.Sprintf(`{"version":0,"role":"computer-seed","logical_size":%d,"chunk_size":4194304,"codec":"zstd"}`, size))
	binary.Write(&encoded, binary.BigEndian, uint32(len(header)))
	encoded.Write(header)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll(bytes.Repeat([]byte{7}, 4<<20), nil)
	encoded.WriteByte(1)
	binary.Write(&encoded, binary.BigEndian, uint64(0))
	binary.Write(&encoded, binary.BigEndian, uint32(4<<20))
	binary.Write(&encoded, binary.BigEndian, uint64(len(compressed)))
	encoded.Write(compressed)
	encoded.WriteByte(255)
	content := encoded.Bytes()
	artifact := SeedArtifact{Object: cas.Descriptor{Digest: sha256sum.DigestBytes(content), SizeBytes: int64(len(content)), MediaType: SeedMediaType}, LogicalBytes: size}
	if err := VerifySeed(t.Context(), bytes.NewReader(content), artifact, size); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"digest", "size", "capacity", "trailing", "truncated", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			a := artifact
			data := content
			capacity := size
			ctx := t.Context()
			switch kind {
			case "digest":
				a.Object.Digest = "sha256:" + strings.Repeat("a", 64)
			case "size":
				a.Object.SizeBytes++
			case "capacity":
				capacity *= 2
			case "trailing":
				data = append(bytes.Clone(content), 0)
			case "truncated":
				data = content[:len(content)-1]
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := VerifySeed(ctx, bytes.NewReader(data), a, capacity); err == nil {
				t.Fatal("invalid seed accepted")
			}
		})
	}
}
