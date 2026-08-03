//go:build linux

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func inspectBuildTree(
	ctx context.Context,
	source io.ReaderAt,
	physicalSize int64,
) (*inspectedArtifact, error) {
	reader, err := newSquashFSArtifactReader(
		ctx,
		source,
		physicalSize,
		buildTreeArtifact,
	)
	if err != nil {
		return nil, fmt.Errorf("open build tree: %w", err)
	}
	tree, err := inspectArtifact(
		ctx,
		reader,
		buildTreeArtifact,
		maxBuildTreeLogicalBytes,
		physicalSize,
	)
	if err != nil {
		return nil, fmt.Errorf("inspect build tree: %w", err)
	}
	if err := validateInspectedBuildTree(ctx, tree); err != nil {
		return nil, err
	}
	return tree, nil
}

func IngestBuildTreeArchive(
	ctx context.Context,
	directory string,
	encoder string,
	archiveDigest string,
	archiveSize int64,
	source io.Reader,
) (_ *BuildTree, returnErr error) {
	if ctx == nil {
		return nil, errors.New("build tree ingestion context is nil")
	}
	if directory == "" ||
		!filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return nil, errors.New("build tree ingestion directory must be an absolute clean path")
	}
	if !sha256DigestPattern.MatchString(archiveDigest) {
		return nil, errors.New("build tree stream digest is not a lowercase SHA-256 digest")
	}
	if archiveSize < 1 || archiveSize > maxBuildTreeStreamBytes {
		return nil, fmt.Errorf(
			"build tree stream size is outside [1,%d]",
			maxBuildTreeStreamBytes,
		)
	}
	if source == nil {
		return nil, errors.New("build tree stream is nil")
	}

	limited := &io.LimitedReader{R: source, N: archiveSize}
	digest := sha256.New()
	reader := io.TeeReader(limited, digest)
	snapshot, err := produceArtifactSnapshot(
		ctx,
		directory,
		buildTreeArtifact,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(destination *os.File) error {
			return encodeSquashFS(ctx, encoder, reader, destination)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("encode build tree stream: %w", err)
	}
	defer func() {
		if snapshot != nil {
			returnErr = errors.Join(returnErr, snapshot.Close())
		}
	}()
	if limited.N != 0 {
		return nil, errors.New("build tree stream is truncated")
	}
	actualDigest := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actualDigest != archiveDigest {
		return nil, fmt.Errorf(
			"build tree stream digest = %s, want %s",
			actualDigest,
			archiveDigest,
		)
	}
	file, err := snapshot.verifierFile()
	if err != nil {
		return nil, err
	}
	inspected, err := inspectBuildTree(ctx, file, snapshot.descriptor.SizeBytes)
	if err != nil {
		return nil, err
	}
	tree, err := newBuildTree(snapshot, inspected, BuildTreeDescriptor{
		Digest:    archiveDigest,
		SizeBytes: archiveSize,
	})
	if err != nil {
		return nil, err
	}
	snapshot = nil
	return tree, nil
}
