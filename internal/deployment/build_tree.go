package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// buildTreeSnapshotMediaType is an in-process snapshot discriminator. It is
// never published, persisted, or used as Program identity.
const buildTreeSnapshotMediaType = "application/vnd.helmr.internal-build-tree.v0+squashfs"

const maxBuildTreeStreamBytes int64 = 11 << 30

type BuildTreeDescriptor struct {
	Digest    string
	SizeBytes int64
}

// BuildTree is the one lease-private, read-only post-lifecycle tree used by
// analysis, Workspace image construction, and Program encoding.
type BuildTree struct {
	content   *artifactSnapshot
	inspected *inspectedArtifact
}

func (tree *BuildTree) Descriptor() (BuildTreeDescriptor, error) {
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return BuildTreeDescriptor{}, errors.New("build tree is closed")
	}
	return BuildTreeDescriptor{
		Digest:    tree.content.descriptor.Digest,
		SizeBytes: tree.content.descriptor.SizeBytes,
	}, nil
}

func IngestBuildTree(
	ctx context.Context,
	directory string,
	descriptor BuildTreeDescriptor,
	source io.Reader,
) (*BuildTree, error) {
	expected := artifactSnapshotDescriptor{
		Digest:    descriptor.Digest,
		SizeBytes: descriptor.SizeBytes,
		MediaType: buildTreeSnapshotMediaType,
	}
	content, err := snapshotArtifact(
		ctx,
		directory,
		buildTreeArtifact,
		expected,
		source,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot build tree: %w", err)
	}
	file, err := content.verifierFile()
	if err != nil {
		return nil, errors.Join(err, content.Close())
	}
	inspected, err := inspectBuildTree(ctx, file, descriptor.SizeBytes)
	if err != nil {
		return nil, errors.Join(err, content.Close())
	}
	return &BuildTree{content: content, inspected: inspected}, nil
}

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
	if err := validateInspectedBuildTree(tree); err != nil {
		return nil, err
	}
	return tree, nil
}

func validateInspectedBuildTree(tree *inspectedArtifact) error {
	if tree == nil {
		return errors.New("build tree inspection is nil")
	}
	if _, exists := tree.entries["helmr"]; exists {
		return errors.New("build tree contains reserved root path \"helmr\"")
	}
	if dependencies, exists := tree.entries["node_modules"]; exists &&
		dependencies.Kind != artifactEntryDirectory {
		return errors.New(
			"build tree root path \"node_modules\" is not a directory",
		)
	}
	if err := validateBuildTreeLinks(tree); err != nil {
		return err
	}
	return nil
}

func validateBuildTreeLinks(tree *inspectedArtifact) error {
	for _, entry := range tree.ordered {
		if entry.Kind != artifactEntrySymlink {
			continue
		}
		if err := validateBuildTreeLink(tree, entry.Path, entry.LinkTarget); err != nil {
			return fmt.Errorf("build tree link %q: %w", entry.Path, err)
		}
	}
	return nil
}

func validateBuildTreeLink(
	tree *inspectedArtifact,
	link string,
	target string,
) error {
	pending := append(
		strings.Split(path.Dir(link), "/"),
		strings.Split(target, "/")...,
	)
	resolved := make([]string, 0, len(pending))
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return errors.New("target escapes the frozen project tree")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(resolved, component), "/")
		entry, exists := tree.entries[candidate]
		if !exists {
			return nil
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return fmt.Errorf("target exceeds %d symbolic-link hops", maxSymlinkHops)
			}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		if entry.Kind != artifactEntryDirectory && len(pending) != 0 {
			return nil
		}
		resolved = append(resolved, component)
	}
	return nil
}

func (tree *BuildTree) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if tree == nil || tree.content == nil {
		return errors.New("build tree is closed")
	}
	return tree.content.LinkInto(directory, name, uid, gid)
}

func (tree *BuildTree) Close() error {
	if tree == nil || tree.content == nil {
		return nil
	}
	err := tree.content.Close()
	tree.content = nil
	tree.inspected = nil
	return err
}
