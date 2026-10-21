//go:build linux

package localcopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"golang.org/x/sys/unix"
)

func borrowArtifact(ctx context.Context, source BorrowedDrive, role artifact.Role, descriptor cas.Descriptor) (_ *Artifact, resultErr error) {
	local := &Artifact{descriptor: descriptor, platform: artifactPlatform{borrowed: source}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, local.Close())
		}
	}()
	spec, err := artifactSpecForRole(role)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactCopyDescriptor(spec, descriptor); err != nil {
		return nil, err
	}
	directory, name, gid, err := source.Open()
	if err != nil {
		return nil, err
	}
	local.platform.directory, local.platform.name = directory, name
	if err := validateArtifactCopyDirectory(directory); err != nil {
		source.Discard()
		return nil, err
	}
	if filepath.Base(name) != name || name == "." || name == ".." || name == "" {
		source.Discard()
		return nil, errors.New("invalid borrowed artifact name")
	}
	identity, err := inspectArtifactCopyAt(int(directory.Fd()), name)
	if err != nil {
		source.Discard()
		return nil, err
	}
	local.platform.identity = identity
	local.verifier, err = openReadOnlyArtifactCopy(directory, name, identity)
	if err != nil {
		if !errors.Is(err, unix.EMFILE) && !errors.Is(err, unix.ENFILE) {
			source.Discard()
		}
		return nil, err
	}
	sealed, err := inspectSealedArtifactCopy(local.verifier, descriptor, Owner{UID: os.Geteuid(), GID: gid}, 0440)
	if err != nil || sealed != identity {
		source.Discard()
		return nil, errors.Join(err, errors.New("invalid shared artifact identity"))
	}
	local.upload, err = openReadOnlyArtifactCopy(directory, name, identity)
	if err != nil {
		if !errors.Is(err, unix.EMFILE) && !errors.Is(err, unix.ENFILE) {
			source.Discard()
		}
		return nil, err
	}
	digest, err := hashArtifactCopy(ctx, local.verifier, descriptor.SizeBytes)
	if err != nil {
		if ctx.Err() == nil {
			source.Discard()
		}
		return nil, err
	}
	if digest != descriptor.Digest {
		source.Discard()
		return nil, cas.ErrDigestMismatch
	}
	if err := validateArtifactCopyPlatform(local); err != nil {
		source.Discard()
		return nil, err
	}
	return local, nil
}
