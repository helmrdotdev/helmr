package computerhost

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func (p *PreparedMachines) seedObject(ctx context.Context, namespace string, d cas.Descriptor, file *os.File) {
	started := time.Now()
	seeded, err := p.ObjectCache.Seed(ctx, namespace, d, io.NewSectionReader(file, 0, d.SizeBytes))
	if seeded || err != nil {
		p.logInfo("worker object cache seed", "namespace", namespace, "size_bytes", d.SizeBytes, "seeded", seeded, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	}
}

func (p *PreparedMachines) seedProgramArtifacts(ctx context.Context, images *programSnapshots, program cas.Descriptor) {
	if p.ObjectCache == nil {
		return
	}
	// These snapshots have been verified, but have not been linked into a VM.
	// Each seed owns an independent inode; later LinkInto may change ownership.
	if images.artifact != nil {
		if file, err := images.artifact.VerifierFile(); err == nil {
			p.seedObject(ctx, "program", program, file)
		}
	}
	if file, descriptor, err := images.runtime.VerifierFile(); err == nil {
		p.seedObject(ctx, "runtime", cas.Descriptor{Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType}, file)
	}
}
