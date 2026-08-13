package deployment

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// buildTreeSnapshotMediaType is an in-process snapshot discriminator. It is
// never published, persisted, or used as Program identity.
const buildTreeSnapshotMediaType = "application/vnd.helmr.internal-build-tree.v0+squashfs"

const maxBuildTreeStreamBytes int64 = 11 << 30

// BuildTree is the one lease-private, read-only post-lifecycle tree used by
// analysis, Workspace image construction, and Program encoding.
type BuildTree struct {
	content    *artifactSnapshot
	inspected  *inspectedArtifact
	descriptor BuildTreeDescriptor
}

// BuildTreeDescriptor identifies the exact post-lifecycle stream accepted
// from the Build guest. It describes that verified stream, not the internal
// SquashFS snapshot used to retain it on the Worker.
type BuildTreeDescriptor struct {
	Digest    string
	SizeBytes int64
}

func newBuildTree(
	content *artifactSnapshot,
	inspected *inspectedArtifact,
	descriptor BuildTreeDescriptor,
) (*BuildTree, error) {
	if content == nil || inspected == nil {
		return nil, errors.New("build tree snapshot is incomplete")
	}
	if inspected.role != buildTreeArtifact {
		return nil, errors.New("build tree snapshot has the wrong artifact role")
	}
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
		return nil, errors.New("build tree stream digest is not a lowercase SHA-256 digest")
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > maxBuildTreeStreamBytes {
		return nil, fmt.Errorf(
			"build tree stream size is outside [1,%d]",
			maxBuildTreeStreamBytes,
		)
	}
	return &BuildTree{
		content:    content,
		inspected:  inspected,
		descriptor: descriptor,
	}, nil
}

func (tree *BuildTree) Descriptor() (BuildTreeDescriptor, error) {
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return BuildTreeDescriptor{}, errors.New("build tree is closed")
	}
	return tree.descriptor, nil
}

func validateInspectedBuildTree(
	ctx context.Context,
	tree *inspectedArtifact,
) error {
	if tree == nil {
		return errors.New("build tree inspection is nil")
	}
	if _, exists := tree.entries["helmr"]; exists {
		if err := validateCompilerBuildTree(ctx, tree); err != nil {
			return err
		}
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

func validateCompilerBuildTree(
	ctx context.Context,
	tree *inspectedArtifact,
) error {
	for _, required := range []string{"helmr"} {
		if _, err := tree.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("compiler build tree: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/compiler-result.json",
		"helmr/config.json",
	} {
		if _, err := tree.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("compiler build tree: %w", err)
		}
	}
	raw, err := tree.read(
		ctx,
		"helmr/compiler-result.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
	}
	result, err := ParseProgramCompilerResult(raw)
	if err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
	}
	generated := make(map[string]struct{}, len(result.Outputs)*2)
	generatedDirectories := make(map[string]struct{}, len(result.Outputs)*2)
	for _, output := range result.Outputs {
		generated[output.ModulePath] = struct{}{}
		generated[output.SourceMapPath] = struct{}{}
		moduleDirectory := path.Dir(output.ModulePath)
		generatedDirectories[moduleDirectory] = struct{}{}
		generatedDirectories[path.Dir(moduleDirectory)] = struct{}{}
	}
	for _, entry := range tree.ordered {
		if entry.Path == "helmr" ||
			entry.Path == "helmr/compiler-result.json" ||
			entry.Path == "helmr/config.json" {
			continue
		}
		if strings.HasPrefix(entry.Path, "helmr/") {
			return fmt.Errorf(
				"compiler build tree contains unknown path %q",
				entry.Path,
			)
		}
		if !hasReservedOutputSegment(entry.Path) {
			continue
		}
		if _, ok := generated[entry.Path]; ok &&
			entry.Kind == artifactEntryRegular {
			continue
		}
		if _, ok := generatedDirectories[entry.Path]; ok &&
			entry.Kind == artifactEntryDirectory {
			continue
		}
		return fmt.Errorf(
			"compiler build tree contains unknown path %q",
			entry.Path,
		)
	}
	if err := verifyProgramCompilerFiles(ctx, tree, result); err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
	}
	return nil
}

func hasReservedOutputSegment(name string) bool {
	for component := range strings.SplitSeq(name, "/") {
		if component == ".helmr" {
			return true
		}
	}
	return false
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

func (tree *BuildTree) Close() error {
	if tree == nil || tree.content == nil {
		return nil
	}
	err := tree.content.Close()
	tree.content = nil
	tree.inspected = nil
	return err
}
