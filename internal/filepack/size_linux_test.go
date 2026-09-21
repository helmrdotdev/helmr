//go:build linux

package filepack

import (
	"context"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackedSizeLimitBoundsActualEncoding(t *testing.T) {
	for _, n := range []int64{0, 1, filepackChunkSize - 1, filepackChunkSize, filepackChunkSize + 1, 2*filepackChunkSize + 4096} {
		t.Run(strconv.FormatInt(n, 10), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source")
			data := make([]byte, n)
			random := rand.NewChaCha8([32]byte{7})
			if _, err := random.Read(data); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			const role = "disk\n\"quoted"
			limit, err := PackedSizeLimit(n, role)
			if err != nil {
				t.Fatal(err)
			}
			out := &countingPackWriter{}
			if _, err := PackTo(context.Background(), file, out, role); err != nil {
				t.Fatal(err)
			}
			if out.n > limit {
				t.Fatalf("encoded %d exceeds %d for logical %d", out.n, limit, n)
			}
		})
	}
}

type countingPackWriter struct{ n int64 }

func (w *countingPackWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

var _ io.Writer = (*countingPackWriter)(nil)

func TestPackedSizeLimitRejectsOverflowAndOversizedHeader(t *testing.T) {
	for _, n := range []int64{-1, math.MaxInt64} {
		if _, err := PackedSizeLimit(n, "disk"); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	if _, err := PackedSizeLimit(4096, strings.Repeat("x", maxFilepackHeader)); err == nil {
		t.Fatal("oversized header accepted")
	}
	const capacity = int64(32 << 30)
	limit, err := PackedSizeLimit(capacity, "computer-disk")
	if err != nil {
		t.Fatal(err)
	}
	if limit < capacity || limit > capacity+capacity/100 {
		t.Fatalf("unexpected dense disk bound: %d", limit)
	}
}
