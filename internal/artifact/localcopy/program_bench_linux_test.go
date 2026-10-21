//go:build linux

package localcopy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

func BenchmarkProgramCopyAndLink(b *testing.B) {
	// Match a representative deployed Program. The source is already local;
	// this measures verification, private copying and pre-exposure writeback.
	body := make([]byte, 349581312)
	for i := range body {
		body[i] = byte(i*31 + i/4096)
	}
	descriptor := cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, body))
	directory := b.TempDir()
	var copyTime, linkTime time.Duration
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		started := time.Now()
		local, err := copyArtifact(b.Context(), directory, artifact.RoleProgram, descriptor, bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		copyTime += time.Since(started)
		started = time.Now()
		if err := local.LinkInto(directory, "program.squashfs", os.Getuid(), os.Getgid()); err != nil {
			_ = local.Close()
			b.Fatal(err)
		}
		linkTime += time.Since(started)
		if err := os.Remove(filepath.Join(directory, "program.squashfs")); err != nil {
			b.Fatal(err)
		}
		if err := local.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(copyTime.Seconds()/float64(b.N), "copy-s/op")
	b.ReportMetric(linkTime.Seconds()/float64(b.N), "link-s/op")
}
