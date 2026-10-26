//go:build linux || darwin

package disk

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// Compare the admitted dirty budgets using the same guest write/flush patterns.
// Counts include all staged objects, including intermediate roots; they are not
// remote upload counts or a save-latency benchmark.
func TestLocalVersionDirtyBudgetObjectCounts(t *testing.T) {
	for _, pattern := range []string{"sequential", "overwrites", "frequent_flush"} {
		t.Run(pattern, func(t *testing.T) {
			counts := make(map[int]int)
			for _, budget := range []int{256, 1024} {
				t.Run(fmt.Sprint(budget), func(t *testing.T) {
					const size = 16 << 20
					parent := t.TempDir()
					base, err := cas.NewFile(filepath.Join(parent, "base"))
					if err != nil {
						t.Fatal(err)
					}
					key := uuid.NewV7().String()
					writer := blockformat.Writer{Source: base, Sink: base, Scope: "fixture", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
					locator, err := writer.Empty(t.Context(), size, 64)
					if err != nil {
						t.Fatal(err)
					}
					root, err := NewVersionRoot(locator, size)
					if err != nil {
						t.Fatal(err)
					}
					cfg := LocalVersionConfig{Directory: filepath.Join(parent, "local"), Base: root, BaseSource: base, Scope: writer.Scope, ActiveKey: key, Keys: writer.Keys, DirtyBlocks: budget, StagedBytes: 64 << 20, PackLimit: blockformat.MinPackLimit}
					local, err := CreateLocalVersion(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer local.Close()
					device := versionDevice{local}
					expected := make([]byte, size)
					writeTimes := make([]time.Duration, 0, size/blockformat.BlockSize)
					for block := range size / blockformat.BlockSize {
						offset := block * blockformat.BlockSize
						if pattern == "overwrites" {
							offset %= 4 << 20
						}
						data := bytes.Repeat([]byte{byte(block%251 + 1)}, blockformat.BlockSize)
						writeStarted := time.Now()
						if n, err := device.WriteAt(t.Context(), data, int64(offset)); err != nil || n != len(data) {
							t.Fatalf("write block %d: %d %v", block, n, err)
						}
						writeTimes = append(writeTimes, time.Since(writeStarted))
						copy(expected[offset:], data)
						if pattern == "frequent_flush" && (block+1)%64 == 0 {
							if err := device.Flush(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := device.Flush(t.Context()); err != nil {
						t.Fatal(err)
					}
					var objects int
					var stagedBytes int64
					if err := filepath.WalkDir(filepath.Join(cfg.Directory, "objects"), func(path string, entry fs.DirEntry, err error) error {
						if err != nil || entry.IsDir() {
							return err
						}
						info, err := entry.Info()
						if err == nil {
							objects++
							stagedBytes += info.Size()
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					counts[budget] = objects
					slices.Sort(writeTimes)
					var writeTotal time.Duration
					for _, elapsed := range writeTimes {
						writeTotal += elapsed
					}
					t.Logf("local_write_total_ms=%.3f local_write_p50_us=%.3f local_write_p95_us=%.3f", float64(writeTotal)/float64(time.Millisecond), float64(writeTimes[len(writeTimes)/2])/float64(time.Microsecond), float64(writeTimes[len(writeTimes)*95/100])/float64(time.Microsecond))
					t.Logf("written_bytes=%d dirty_blocks=%d staged_objects=%d staged_bytes=%d", size, budget, objects, stagedBytes)
					if err := local.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := OpenLocalVersion(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					data := make([]byte, blockformat.BlockSize)
					for offset := 0; offset < size; offset += len(data) {
						if n, err := reopened.ReadAt(t.Context(), data, int64(offset)); err != nil || n != len(data) || !bytes.Equal(data, expected[offset:offset+len(data)]) {
							t.Fatalf("reopened data differs at %d: %d %v", offset, n, err)
						}
					}
				})
			}
			if t.Failed() {
				return
			}
			if pattern == "frequent_flush" {
				if counts[1024] != counts[256] {
					t.Fatalf("guest flushes should control cuts: %v", counts)
				}
			} else if counts[1024] >= counts[256] {
				t.Fatalf("larger dirty budget did not reduce objects: %v", counts)
			}
		})
	}
}
