//go:build linux

package deployment

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func inspectBuildTree(
	ctx context.Context,
	source io.ReaderAt,
	physicalSize int64,
) (*artifact.Tree, error) {
	reader, err := verify.NewSquashFSReader(
		ctx,
		source,
		physicalSize,
		artifact.RoleBuildTree,
	)
	if err != nil {
		return nil, fmt.Errorf("open build tree: %w", err)
	}
	tree, err := artifact.Inspect(
		ctx,
		reader,
		artifact.RoleBuildTree,
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
	if !sha256sum.ValidDigest(archiveDigest) {
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
	content, err := snapshot.Produce(
		ctx,
		directory,
		artifact.RoleBuildTree,
		snapshot.Owner{UID: os.Geteuid(), GID: os.Getegid()},
		false,
		func(destination *os.File) error {
			return encodeSquashFS(ctx, encoder, reader, destination)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("encode build tree stream: %w", err)
	}
	defer func() {
		if content != nil {
			returnErr = errors.Join(returnErr, content.Close())
		}
	}()
	if limited.N != 0 {
		return nil, errors.New("build tree stream is truncated")
	}
	actualDigest := sha256sum.FormatDigest(digest.Sum(nil))
	if actualDigest != archiveDigest {
		return nil, fmt.Errorf(
			"build tree stream digest = %s, want %s",
			actualDigest,
			archiveDigest,
		)
	}
	file, err := content.VerifierFile()
	if err != nil {
		return nil, err
	}
	inspected, err := inspectBuildTree(ctx, file, content.Descriptor().SizeBytes)
	if err != nil {
		return nil, err
	}
	tree, err := newBuildTree(content, inspected, BuildTreeDescriptor{
		Digest:    archiveDigest,
		SizeBytes: archiveSize,
	})
	if err != nil {
		return nil, err
	}
	content = nil
	return tree, nil
}
