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

	snapshot, err := produceArtifactSnapshot(
		ctx,
		leaseDirectory,
		role,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(destination *os.File) error {
			reader, writer := io.Pipe()
			writeResult := make(chan error, 1)
			go func() {
				err := writeTreeArchive(ctx, writer, role, entries, allowEmpty)
				_ = writer.CloseWithError(err)
				writeResult <- err
			}()
			encodeErr := encodeSquashFS(ctx, encoder, reader, destination)
			closeErr := reader.Close()
			writeErr := <-writeResult
			return errors.Join(encodeErr, closeErr, writeErr)
		},
	)
	if err != nil {
		return nil, err
	}
	snapshot.platform.removeDirectory = true
	removeLease = false
	return snapshot, nil
}
