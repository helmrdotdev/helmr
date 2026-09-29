package snapshot

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
	content, err := snapshotArtifact(
		ctx,
		directory,
		artifact.RoleRuntime,
		artifactSnapshotDescriptor{
			Digest:    descriptor.Digest,
			MediaType: descriptor.MediaType,
			SizeBytes: descriptor.SizeBytes,
		},
		source,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot runtime artifact: %w", err)
	}
	return &Runtime{
		descriptor: descriptor,
		content:    content,
	}, nil
}

func (snapshot *Runtime) VerifierFile() (*os.File, artifact.RuntimeDescriptor, error) {
	if snapshot == nil || snapshot.content == nil {
		return nil, artifact.RuntimeDescriptor{}, errors.New("runtime artifact snapshot is closed")
	}
	// snapshotArtifact binds the descriptor to a sealed inode; the isolated
	// child re-reads all bytes while the parent retains this outer identity.
	file, err := snapshot.content.VerifierFile()
	if err != nil {
		return nil, artifact.RuntimeDescriptor{}, err
	}
	return file, snapshot.descriptor, nil
}

func (snapshot *Runtime) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("runtime artifact snapshot is closed")
	}
	return snapshot.content.LinkInto(directory, name, uid, gid)
}

// PublishVerified stores the snapshot bytes under the descriptor it was taken
// for. It publishes only after in-process verification produced verified for
// this snapshot, and rejects a verified index that does not match the
// descriptor. The only caller is verify.PublishPlatformRuntime.
func (snapshot *Runtime) PublishVerified(
	ctx context.Context,
	store cas.ImmutableStore,
	verified artifact.RuntimeIndex,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("runtime artifact snapshot is closed")
	}
	if verified.Architecture != snapshot.descriptor.Architecture ||
		verified.RuntimeContract != snapshot.descriptor.RuntimeContract {
		return errors.New("verified Platform Runtime does not match its descriptor")
	}
	if err := validateArtifactSnapshotPlatform(snapshot.content); err != nil {
		return err
	}
	_, err := store.Publish(ctx, cas.Descriptor{
		Digest: snapshot.descriptor.Digest, MediaType: snapshot.descriptor.MediaType,
		SizeBytes: snapshot.descriptor.SizeBytes,
	}, snapshot.content.upload)
	return err
}

func (snapshot *Runtime) Close() error {
	if snapshot == nil || snapshot.content == nil {
		return nil
	}
	err := snapshot.content.Close()
	snapshot.content = nil
	return err
}
