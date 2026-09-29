package builder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// maxBuildTreeStreamBytes is the producer-side bound for the installed Program
// tree before it is encoded and deeply verified as a Program artifact.
const maxBuildTreeStreamBytes int64 = 11 << 30

// BuildTree is the one lease-private, read-only post-lifecycle tree used by
// analysis, Computer image construction, and Program encoding.
type BuildTree struct {
	content    *snapshot.Artifact
	inspected  *artifact.Tree
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
	content *snapshot.Artifact,
	inspected *artifact.Tree,
	descriptor BuildTreeDescriptor,
) (*BuildTree, error) {
	if content == nil || inspected == nil {
		return nil, errors.New("build tree snapshot is incomplete")
	}
	if inspected.Role() != artifact.RoleBuildTree {
		return nil, errors.New("build tree snapshot has the wrong artifact role")
	}
	if !sha256sum.ValidDigest(descriptor.Digest) {
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
	tree *artifact.Tree,
) error {
	if tree == nil {
		return errors.New("build tree inspection is nil")
	}
	if _, exists := tree.Lookup("helmr"); exists {
		if err := validateCompilerBuildTree(ctx, tree); err != nil {
			return err
		}
	}
	if dependencies, exists := tree.Lookup("node_modules"); exists &&
		dependencies.Kind != artifact.EntryDirectory {
		return errors.New(
			"build tree root path \"node_modules\" is not a directory",
		)
	}
	if err := validateBuildTreeLinks(tree.Entries(), tree.Lookup); err != nil {
		return err
	}
	return nil
}

func validateCompilerBuildTree(
	ctx context.Context,
	tree *artifact.Tree,
) error {
	for _, required := range []string{"helmr"} {
		if _, err := tree.Require(required, artifact.EntryDirectory); err != nil {
			return fmt.Errorf("compiler build tree: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/compiler-result.json",
		"helmr/config.json",
	} {
		if _, err := tree.Require(required, artifact.EntryRegular); err != nil {
			return fmt.Errorf("compiler build tree: %w", err)
		}
	}
	raw, err := tree.Read(
		ctx,
		"helmr/compiler-result.json",
		artifact.MaxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
	}
	result, err := artifact.ParseProgramCompilerResult(raw)
	if err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
	}
	for _, entry := range tree.Entries() {
		if strings.HasPrefix(entry.Path, "helmr/") && !artifact.IsGeneratedProgramEntry(entry) && entry.Path != "helmr/compiler-result.json" && entry.Path != "helmr/config.json" {
			return fmt.Errorf("compiler build tree contains unknown path %q", entry.Path)
		}
	}
	if err := artifact.VerifyProgramCompilerFiles(ctx, tree, result); err != nil {
		return fmt.Errorf("compiler build tree: %w", err)
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
