//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func buildManagerProject(
	ctx context.Context,
	directory string,
	encoder string,
	source DependencySource,
) (_ *managerProject, returnErr error) {
	if ctx == nil {
		return nil, errors.New("manager project context is nil")
	}
	if directory == "" ||
		!filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return nil, errors.New(
			"manager project directory must be an absolute clean path",
		)
	}
	if err := validateProgramEncoder(encoder); err != nil {
		return nil, err
	}

	leaseDirectory, err := os.MkdirTemp(directory, ".helmr-project-")
	if err != nil {
		return nil, fmt.Errorf("create manager project lease: %w", err)
	}
	if err := os.Chmod(leaseDirectory, 0700); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set manager project lease mode: %w", err),
			os.Remove(leaseDirectory),
		)
	}
	removeLease := true
	defer func() {
		if removeLease {
			returnErr = errors.Join(returnErr, os.RemoveAll(leaseDirectory))
		}
	}()

	archivePath := filepath.Join(leaseDirectory, "project.tar")
	archive, err := os.OpenFile(
		archivePath,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0600,
	)
	if err != nil {
		return nil, fmt.Errorf("create manager project archive: %w", err)
	}
	defer func() {
		if archive != nil {
			returnErr = errors.Join(returnErr, archive.Close())
		}
	}()
	if err := writeManagerProjectArchive(ctx, archive, source); err != nil {
		return nil, err
	}
	if err := archive.Sync(); err != nil {
		return nil, fmt.Errorf("sync manager project archive: %w", err)
	}
	info, err := archive.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect manager project archive: %w", err)
	}
	if info.Size() < 1 || info.Size() > maxManagerProjectBytes {
		return nil, fmt.Errorf(
			"manager project archive size is outside [1,%d]",
			maxManagerProjectBytes,
		)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind manager project archive: %w", err)
	}

	spec := artifactSnapshotSpec{
		label:     "manager project",
		mediaType: ManagerProjectMediaType,
		maxBytes:  maxManagerProjectBytes,
	}
	snapshot, err := produceArtifactSnapshotWithSpec(
		ctx,
		leaseDirectory,
		spec,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(destination *os.File) error {
			return encodeSquashFS(ctx, encoder, archive, destination)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("encode manager project: %w", err)
	}
	if err := archive.Close(); err != nil {
		archive = nil
		return nil, errors.Join(
			fmt.Errorf("close manager project archive: %w", err),
			snapshot.Close(),
		)
	}
	archive = nil
	if err := os.Remove(archivePath); err != nil {
		return nil, errors.Join(
			fmt.Errorf("remove manager project archive: %w", err),
			snapshot.Close(),
		)
	}

	snapshot.platform.removeDirectory = true
	removeLease = false
	descriptor := snapshot.descriptor
	return &managerProject{
		descriptor: ManagerArtifact{
			Digest:    descriptor.Digest,
			MediaType: descriptor.MediaType,
			SizeBytes: descriptor.SizeBytes,
		},
		snapshot: snapshot,
	}, nil
}
