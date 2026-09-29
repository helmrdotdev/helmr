package builder

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"path"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// buildTreeSource is a sealed selection over one verified buildTree. Its
// exported facts are copies; archive bytes can only be emitted again from the
// same read-only tree and exact selected entries.
type buildTreeSource struct {
	tree       *buildTree
	entries    []artifact.Entry
	paths      []sourceArchivePath
	descriptor sourceArchiveDescriptor
}

// selectImageSource expands every source-copy root in plan against the exact
// verified post-lifecycle buildTree and computes the canonical archive facts
// required before a local producer accepts the source tree.
func (tree *buildTree) selectImageSource(
	ctx context.Context,
	plan definition.ImageBuild,
) (*buildTreeSource, error) {
	if ctx == nil {
		return nil, errors.New("image source selection context is nil")
	}
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return nil, errors.New("build tree is closed")
	}
	if len(plan.Images) == 0 {
		return nil, errors.New("image source plan has no images")
	}
	if err := definition.ValidateImageBuild(plan, plan.Images[0].Platform.Architecture); err != nil {
		return nil, fmt.Errorf("validate image source plan: %w", err)
	}

	fileRoots, directoryRoots := imageSourceRoots(plan)
	selected, err := selectBuildTreeEntries(tree.inspected, fileRoots, directoryRoots)
	if err != nil {
		return nil, err
	}
	if len(selected) > maxSourceArchiveEntries {
		return nil, fmt.Errorf(
			"image source archive entry count exceeds %d",
			maxSourceArchiveEntries,
		)
	}
	paths := make([]sourceArchivePath, len(selected))
	for index, entry := range selected {
		kind, err := imageSourcePathKind(entry.Kind)
		if err != nil {
			return nil, err
		}
		paths[index] = sourceArchivePath{Path: entry.Path, Kind: kind}
	}

	digest := sha256.New()
	counter := &archiveByteCounter{destination: digest}
	if err := writeSelectedBuildTreeArchive(ctx, counter, tree.inspected, selected); err != nil {
		return nil, err
	}
	descriptor := sourceArchiveDescriptor{
		ArchiveDigest:    sha256sum.FormatDigest(digest.Sum(nil)),
		ArchiveSizeBytes: counter.written,
		ArchiveEntries:   len(selected),
		PathSetDigest:    sourcePathSetDigest(paths),
	}
	if descriptor.ArchiveSizeBytes < 1 ||
		descriptor.ArchiveSizeBytes > maxSourceArchiveBytes {
		return nil, errors.New("image source archive size is outside the v0 contract")
	}
	entryCopy := make([]artifact.Entry, len(selected))
	copy(entryCopy, selected)
	pathCopy := make([]sourceArchivePath, len(paths))
	copy(pathCopy, paths)
	return &buildTreeSource{
		tree:       tree,
		entries:    entryCopy,
		paths:      pathCopy,
		descriptor: descriptor,
	}, nil
}

func (source *buildTreeSource) Descriptor() (sourceArchiveDescriptor, error) {
	if source == nil || source.tree == nil || len(source.entries) != len(source.paths) {
		return sourceArchiveDescriptor{}, errors.New("image source selection is invalid")
	}
	return source.descriptor, nil
}

func (source *buildTreeSource) Paths() ([]sourceArchivePath, error) {
	if source == nil || source.tree == nil || len(source.entries) != len(source.paths) {
		return nil, errors.New("image source selection is invalid")
	}
	paths := make([]sourceArchivePath, len(source.paths))
	copy(paths, source.paths)
	return paths, nil
}

// WriteTo emits the exact archive measured during selection. It recomputes and
// verifies the complete descriptor so a closed or changed source cannot be
// sent under previously admitted facts.
func (source *buildTreeSource) WriteTo(
	ctx context.Context,
	destination io.Writer,
) error {
	if ctx == nil {
		return errors.New("image source archive context is nil")
	}
	if destination == nil {
		return errors.New("image source archive destination is nil")
	}
	if source == nil || source.tree == nil ||
		source.tree.content == nil || source.tree.inspected == nil ||
		len(source.entries) != len(source.paths) {
		return errors.New("image source selection is unavailable")
	}
	digest := sha256.New()
	counter := &archiveByteCounter{destination: io.MultiWriter(destination, digest)}
	if err := writeSelectedBuildTreeArchive(
		ctx,
		counter,
		source.tree.inspected,
		source.entries,
	); err != nil {
		return err
	}
	actualDigest := sha256sum.FormatDigest(digest.Sum(nil))
	if actualDigest != source.descriptor.ArchiveDigest ||
		counter.written != source.descriptor.ArchiveSizeBytes {
		return errors.New("image source archive changed after admission")
	}
	return nil
}

func imageSourceRoots(plan definition.ImageBuild) (map[string]struct{}, map[string]struct{}) {
	files := make(map[string]struct{})
	directories := make(map[string]struct{})
	for _, image := range plan.Images {
		for _, step := range image.Steps {
			switch {
			case step.CopySourceFile != nil:
				files[step.CopySourceFile.Path] = struct{}{}
			case step.CopySourceDir != nil:
				directories[step.CopySourceDir.Path] = struct{}{}
			}
		}
	}
	return files, directories
}

func selectBuildTreeEntries(
	tree *artifact.Tree,
	fileRoots map[string]struct{},
	directoryRoots map[string]struct{},
) ([]artifact.Entry, error) {
	selected := make(map[string]artifact.Entry)
	for root := range fileRoots {
		if buildTreeImageSourceReserved(root) {
			return nil, fmt.Errorf("image source root %q is platform compiler output", root)
		}
		entry, err := tree.Require(root, artifact.EntryRegular)
		if err != nil {
			return nil, fmt.Errorf("image source file: %w", err)
		}
		selected[root] = entry
		if err := selectBuildTreeAncestors(tree, selected, root); err != nil {
			return nil, err
		}
	}
	for root := range directoryRoots {
		if buildTreeImageSourceReserved(root) {
			return nil, fmt.Errorf("image source root %q is platform compiler output", root)
		}
		if root != "." {
			entry, err := tree.Require(root, artifact.EntryDirectory)
			if err != nil {
				return nil, fmt.Errorf("image source directory: %w", err)
			}
			selected[root] = entry
			if err := selectBuildTreeAncestors(tree, selected, root); err != nil {
				return nil, err
			}
		}
		for _, entry := range tree.Entries() {
			if entry.Path == "." || buildTreeImageSourceReserved(entry.Path) {
				continue
			}
			if root == "." || strings.HasPrefix(entry.Path, root+"/") {
				selected[entry.Path] = entry
			}
		}
	}

	entries := make([]artifact.Entry, 0, len(selected))
	for _, entry := range selected {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right artifact.Entry) int {
		return strings.Compare(left.Path, right.Path)
	})
	return entries, nil
}

func selectBuildTreeAncestors(
	tree *artifact.Tree,
	selected map[string]artifact.Entry,
	name string,
) error {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if buildTreeImageSourceReserved(parent) {
			return fmt.Errorf("image source path %q descends from platform compiler output", name)
		}
		entry, err := tree.Require(parent, artifact.EntryDirectory)
		if err != nil {
			return fmt.Errorf("image source ancestor: %w", err)
		}
		selected[parent] = entry
	}
	return nil
}

func buildTreeImageSourceReserved(name string) bool {
	return name == "helmr" || strings.HasPrefix(name, "helmr/")
}

func imageSourcePathKind(kind artifact.EntryKind) (sourcePathKind, error) {
	switch kind {
	case artifact.EntryRegular:
		return sourcePathFile, nil
	case artifact.EntryDirectory:
		return sourcePathDirectory, nil
	case artifact.EntrySymlink:
		return sourcePathSymlink, nil
	default:
		return "", fmt.Errorf("image source artifact kind %q is unsupported", kind)
	}
}

func writeSelectedBuildTreeArchive(
	ctx context.Context,
	destination io.Writer,
	tree *artifact.Tree,
	selected []artifact.Entry,
) error {
	return writeTreeArchive(
		ctx,
		destination,
		artifact.RoleBuildTree,
		selectedBuildTreeEntrySequence(ctx, tree, selected),
		true,
	)
}

func selectedBuildTreeEntrySequence(
	ctx context.Context,
	tree *artifact.Tree,
	selected []artifact.Entry,
) iter.Seq2[treeEntry, error] {
	return func(yield func(treeEntry, error) bool) {
		for _, entry := range selected {
			if err := ctx.Err(); err != nil {
				yield(treeEntry{}, err)
				return
			}
			projected := treeEntry{
				Path:       entry.Path,
				Kind:       entry.Kind,
				Mode:       entry.Mode,
				LinkTarget: entry.LinkTarget,
			}
			if entry.Kind == artifact.EntryRegular {
				projected.SizeBytes = entry.SizeBytes
				content, err := tree.Open(ctx, entry.Path)
				if err != nil {
					yield(treeEntry{}, fmt.Errorf("open image source path %q: %w", entry.Path, err))
					return
				}
				projected.Content = content
				if !yield(projected, nil) {
					_ = content.Close()
					return
				}
				if err := content.Close(); err != nil {
					yield(treeEntry{}, fmt.Errorf("close image source path %q: %w", entry.Path, err))
					return
				}
				continue
			}
			if !yield(projected, nil) {
				return
			}
		}
	}
}

type archiveByteCounter struct {
	destination io.Writer
	written     int64
}

func (writer *archiveByteCounter) Write(buffer []byte) (int, error) {
	count, err := writer.destination.Write(buffer)
	writer.written += int64(count)
	return count, err
}
