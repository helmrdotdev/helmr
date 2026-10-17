//go:build linux || darwin

package disk

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

func BenchmarkVersionDeviceWrite(b *testing.B) {
	for _, size := range []int{128 << 10, 1 << 20} {
		for _, device := range []bool{true, false} {
			b.Run(fmt.Sprintf("bytes=%d/device=%t", size, device), func(b *testing.B) {
				parent := b.TempDir()
				store, err := cas.NewFile(filepath.Join(parent, "base"))
				if err != nil {
					b.Fatal(err)
				}
				key := uuid.NewV7().String()
				writer := blockformat.Writer{Source: store, Sink: store, Scope: "benchmark", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
				locator, err := writer.Empty(b.Context(), 4<<20, 64)
				if err != nil {
					b.Fatal(err)
				}
				root, err := NewVersionRoot(locator, 4<<20)
				if err != nil {
					b.Fatal(err)
				}
				p, err := CreateLocalVersion(b.Context(), LocalVersionConfig{Directory: filepath.Join(parent, "local"), Base: root, BaseSource: store, Scope: writer.Scope, ActiveKey: key, Keys: writer.Keys, DirtyBlocks: 1024, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit})
				if err != nil {
					b.Fatal(err)
				}
				defer p.Close()
				data := bytes.Repeat([]byte{1}, size)
				write := p.WriteAt
				if device {
					write = (versionDevice{p}).WriteAt
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if n, err := write(b.Context(), data, 0); err != nil || n != len(data) {
						b.Fatalf("write: %d %v", n, err)
					}
				}
			})
		}
	}
}
