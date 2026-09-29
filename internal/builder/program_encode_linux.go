//go:build linux

package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
)

func encodeProgramTree(
	ctx context.Context,
	directory string,
	encoder string,
	role artifact.Role,
	entries iter.Seq2[treeEntry, error],
	allowEmpty bool,
) (_ *snapshot.Artifact, returnErr error) {
	if directory == "" ||
		!filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return nil, errors.New("program encoding directory must be an absolute clean path")
	}
	if err := validateProgramEncoder(encoder); err != nil {
		return nil, err
	}
	leaseDirectory, err := os.MkdirTemp(directory, ".helmr-program-")
	if err != nil {
		return nil, fmt.Errorf("create program encoding lease: %w", err)
	}
	if err := os.Chmod(leaseDirectory, 0700); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set program encoding lease mode: %w", err),
			os.Remove(leaseDirectory),
		)
	}
	removeLease := true
	defer func() {
		if removeLease {
			returnErr = errors.Join(returnErr, os.RemoveAll(leaseDirectory))
		}
	}()

	content, err := snapshot.Produce(
		ctx,
		leaseDirectory,
		role,
		snapshot.Owner{UID: os.Geteuid(), GID: os.Getegid()},
		true,
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
	removeLease = false
	return content, nil
}
