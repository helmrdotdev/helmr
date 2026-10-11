package localcopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

type Runtime struct {
	descriptor artifact.RuntimeDescriptor
	content    *Artifact
}

func CopyRuntime(
	ctx context.Context,
	directory string,
	descriptor artifact.RuntimeDescriptor,
	source io.Reader,
) (*Runtime, error) {
	if err := artifact.ValidateRuntimeDescriptor(descriptor); err != nil {
		return nil, err
	}
	if descriptor.SizeBytes > artifact.MaxRuntimePhysicalBytes {
		return nil, fmt.Errorf(
			"runtime artifact size exceeds %d bytes",
			artifact.MaxRuntimePhysicalBytes,
		)
	}
	content, err := copyArtifact(
		ctx,
		directory,
		artifact.RoleRuntime,
		cas.Descriptor{
			Digest:    descriptor.Digest,
			MediaType: descriptor.MediaType,
			SizeBytes: descriptor.SizeBytes,
		},
		source,
	)
	if err != nil {
		return nil, fmt.Errorf("copy runtime artifact: %w", err)
	}
	return &Runtime{
		descriptor: descriptor,
		content:    content,
	}, nil
}

func (local *Runtime) VerifierFile() (*os.File, artifact.RuntimeDescriptor, error) {
	if local == nil || local.content == nil {
		return nil, artifact.RuntimeDescriptor{}, errors.New("runtime local artifact copy is closed")
	}
	// copyArtifact binds the descriptor to a sealed inode; the isolated
	// child re-reads all bytes while the parent retains this outer identity.
	file, err := local.content.VerifierFile()
	if err != nil {
		return nil, artifact.RuntimeDescriptor{}, err
	}
	return file, local.descriptor, nil
}

func (local *Runtime) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if local == nil || local.content == nil {
		return errors.New("runtime local artifact copy is closed")
	}
	return local.content.LinkInto(directory, name, uid, gid)
}

// PublishVerified stores the local bytes under the descriptor it was taken
// for. It publishes only after in-process verification produced verified for
// this local, and rejects a verified index that does not match the
// descriptor. The only caller is verify.PublishPlatformRuntime.
func (local *Runtime) PublishVerified(
	ctx context.Context,
	store cas.ImmutableStore,
	verified artifact.RuntimeIndex,
) error {
	if local == nil || local.content == nil {
		return errors.New("runtime local artifact copy is closed")
	}
	if verified.Architecture != local.descriptor.Architecture ||
		verified.RuntimeContract != local.descriptor.RuntimeContract {
		return errors.New("verified Platform Runtime does not match its descriptor")
	}
	if err := validateArtifactCopyPlatform(local.content); err != nil {
		return err
	}
	_, err := store.Publish(ctx, cas.Descriptor{
		Digest: local.descriptor.Digest, MediaType: local.descriptor.MediaType,
		SizeBytes: local.descriptor.SizeBytes,
	}, local.content.upload)
	return err
}

func (local *Runtime) Close() error {
	if local == nil || local.content == nil {
		return nil
	}
	err := local.content.Close()
	local.content = nil
	return err
}
