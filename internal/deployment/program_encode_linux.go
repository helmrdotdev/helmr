//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
)

func encodeProgramTree(
	ctx context.Context,
	directory string,
	encoder string,
	role artifactRole,
	entries iter.Seq2[treeEntry, error],
	allowEmpty bool,
) (_ *artifactSnapshot, returnErr error) {
	if directory == "" ||
		!filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return nil, errors.New("Program encoding directory must be an absolute clean path")
	}
	if err := validateProgramEncoder(encoder); err != nil {
		return nil, err
	}
	leaseDirectory, err := os.MkdirTemp(directory, ".helmr-program-")
	if err != nil {
		return nil, fmt.Errorf("create Program encoding lease: %w", err)
	}
	if err := os.Chmod(leaseDirectory, 0700); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set Program encoding lease mode: %w", err),
			os.Remove(leaseDirectory),
		)
	}
	removeLease := true
	defer func() {
		if removeLease {
			returnErr = errors.Join(returnErr, os.RemoveAll(leaseDirectory))
		}
	}()

	archivePath := filepath.Join(leaseDirectory, "tree.tar")
	archive, err := os.OpenFile(
		archivePath,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0600,
	)
	if err != nil {
		return nil, fmt.Errorf("create Program archive: %w", err)
	}
	defer func() {
		if archive != nil {
			returnErr = errors.Join(returnErr, archive.Close())
		}
	}()
	if err := writeTreeArchive(ctx, archive, role, entries, allowEmpty); err != nil {
		return nil, err
	}
	if err := archive.Sync(); err != nil {
		return nil, fmt.Errorf("sync Program archive: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind Program archive: %w", err)
	}

	snapshot, err := produceArtifactSnapshot(
		ctx,
		leaseDirectory,
		role,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(destination *os.File) error {
			return encodeSquashFS(ctx, encoder, archive, destination)
		},
	)
	if err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		archive = nil
		return nil, errors.Join(
			fmt.Errorf("close Program archive: %w", err),
			snapshot.Close(),
		)
	}
	archive = nil
	if err := os.Remove(archivePath); err != nil {
		return nil, errors.Join(
			fmt.Errorf("remove Program archive: %w", err),
			snapshot.Close(),
		)
	}
	snapshot.platform.removeDirectory = true
	removeLease = false
	return snapshot, nil
}
