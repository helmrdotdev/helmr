package computerhost

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/localcopy"
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
	// Seeds own their inodes. Shared drive seeds retain host ownership at link time.
	if images.artifact != nil {
		if file, err := images.artifact.VerifierFile(); err == nil {
			p.seedObject(ctx, "program", program, file)
		}
	}
	if file, descriptor, err := images.runtime.VerifierFile(); err == nil {
		p.seedObject(ctx, "runtime", cas.Descriptor{Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType}, file)
	}
}

func (p *PreparedMachines) readProgramArtifact(ctx context.Context, directory string, descriptor artifact.ProgramDescriptor) (*localcopy.Program, error) {
	if lease := p.ObjectCache.Borrow(ctx, "program", cas.Descriptor(descriptor)); lease != nil {
		local, err := localcopy.BorrowProgram(ctx, lease, descriptor)
		p.logInfo("artifact borrowed", "role", "program", "digest", descriptor.Digest, "error", errorString(err))
		if err == nil {
			return local, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return localcopy.ReadProgram(ctx, p.ObjectCache.Reader("program", p.CAS), directory, descriptor)
}
func (p *PreparedMachines) readRuntimeArtifact(ctx context.Context, directory string, descriptor artifact.RuntimeDescriptor) (*localcopy.Runtime, error) {
	d := cas.Descriptor{Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType}
	if lease := p.ObjectCache.Borrow(ctx, "runtime", d); lease != nil {
		local, err := localcopy.BorrowRuntime(ctx, lease, descriptor)
		p.logInfo("artifact borrowed", "role", "runtime", "digest", descriptor.Digest, "error", errorString(err))
		if err == nil {
			return local, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return localcopy.ReadRuntime(ctx, p.ObjectCache.Reader("runtime", p.PlatformStore), directory, descriptor)
}
