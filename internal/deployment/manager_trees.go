package deployment

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const managerObjectDirectory = "/usr/lib/helmr/manager-release/objects/sha256"

// ManagerTrees opens certified package-manager trees installed by the Platform
// release. It has no network or publication authority.
type ManagerTrees struct{}

func (trees *ManagerTrees) Snapshot(
	ctx context.Context,
	directory string,
	manager Manager,
) (*ArtifactSnapshot, error) {
	if trees == nil {
		return nil, errors.New("manager trees are required")
	}
	if ctx == nil {
		return nil, errors.New("manager tree context is nil")
	}
	if err := validateManager(manager); err != nil {
		return nil, err
	}
	source, err := openReleaseFileExact(
		filepath.Join(
			managerObjectDirectory,
			strings.TrimPrefix(manager.Tree.Digest, "sha256:"),
		),
		"manager tree",
		manager.Tree.SizeBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	content, snapshotErr := snapshotArtifact(
		ctx,
		directory,
		managerArtifact,
		artifactSnapshotDescriptor{
			Digest:    manager.Tree.Digest,
			SizeBytes: manager.Tree.SizeBytes,
			MediaType: manager.Tree.MediaType,
		},
		source,
	)
	closeErr := source.Close()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if closeErr != nil {
		_ = content.Close()
		return nil, fmt.Errorf("close manager tree: %w", closeErr)
	}
	return &ArtifactSnapshot{content: content}, nil
}
