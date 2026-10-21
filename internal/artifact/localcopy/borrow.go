package localcopy

import (
	"context"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// BorrowedDrive retains a cache pin until Close. Open transfers ownership of
// the directory descriptor, never the backing inode, to this consumer.
type BorrowedDrive interface {
	Open() (*os.File, string, int, error)
	Discard()
	Close() error
}

func BorrowProgram(ctx context.Context, source BorrowedDrive, descriptor artifact.ProgramDescriptor) (*Program, error) {
	content, err := borrowArtifact(ctx, source, artifact.RoleProgram, cas.Descriptor(descriptor))
	if err != nil {
		return nil, err
	}
	return &Program{content: content}, nil
}
func BorrowRuntime(ctx context.Context, source BorrowedDrive, descriptor artifact.RuntimeDescriptor) (*Runtime, error) {
	if err := artifact.ValidateRuntimeDescriptor(descriptor); err != nil {
		_ = source.Close()
		return nil, err
	}
	content, err := borrowArtifact(ctx, source, artifact.RoleRuntime, cas.Descriptor{Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType})
	if err != nil {
		return nil, err
	}
	return &Runtime{descriptor: descriptor, content: content}, nil
}
