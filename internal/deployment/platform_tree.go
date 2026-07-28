package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type platformTree struct {
	artifact *artifactSnapshot
}

var errCopyExceedsLimit = errors.New("copy exceeds its byte limit")

func encodePlatformTree(
	ctx context.Context,
	workDir string,
	encoder string,
	role artifactRole,
	root string,
) (*platformTree, error) {
	artifact, err := encodeProgramTree(
		ctx,
		workDir,
		encoder,
		role,
		filesystemTreeEntries(ctx, root),
		false,
	)
	if err != nil {
		return nil, err
	}
	return &platformTree{artifact: artifact}, nil
}

func (tree *platformTree) descriptor() (ArtifactDescriptor, error) {
	if tree == nil || tree.artifact == nil {
		return ArtifactDescriptor{}, errors.New("Platform tree is closed")
	}
	return platformSnapshotDescriptor(tree.artifact.descriptor), nil
}

func (tree *platformTree) validate(
	ctx context.Context,
	expectation PlatformArtifactExpectation,
) error {
	if tree == nil || tree.artifact == nil {
		return errors.New("Platform tree is closed")
	}
	file, err := tree.artifact.verifierFile()
	if err != nil {
		return err
	}
	descriptor := platformSnapshotDescriptor(tree.artifact.descriptor)
	_, err = InspectPlatformArtifact(ctx, file, descriptor, expectation)
	return err
}

func platformSnapshotDescriptor(value artifactSnapshotDescriptor) ArtifactDescriptor {
	return ArtifactDescriptor{
		Digest:    value.Digest,
		MediaType: value.MediaType,
		SizeBytes: value.SizeBytes,
	}
}

func (tree *platformTree) publish(
	ctx context.Context,
	store cas.ImmutableStore,
) (cas.Object, error) {
	if tree == nil || tree.artifact == nil {
		return cas.Object{}, errors.New("Platform tree is closed")
	}
	if store == nil {
		return cas.Object{}, errors.New("Platform Artifact store is required")
	}
	expected := cas.Descriptor{
		Digest:    tree.artifact.descriptor.Digest,
		MediaType: tree.artifact.descriptor.MediaType,
		SizeBytes: tree.artifact.descriptor.SizeBytes,
	}
	return store.Publish(ctx, expected, tree.artifact.upload)
}

func (tree *platformTree) Close() error {
	if tree == nil || tree.artifact == nil {
		return nil
	}
	err := tree.artifact.Close()
	tree.artifact = nil
	return err
}

func filesystemTreeEntries(
	ctx context.Context,
	root string,
) iter.Seq2[treeEntry, error] {
	return func(yield func(treeEntry, error) bool) {
		root = filepath.Clean(root)
		if !filepath.IsAbs(root) {
			yield(treeEntry{}, errors.New("Platform tree root is not absolute"))
			return
		}
		err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			if relative == "." {
				return nil
			}
			relative = filepath.ToSlash(relative)
			info, err := os.Lstat(name)
			if err != nil {
				return err
			}
			value := treeEntry{Path: relative}
			switch {
			case info.Mode().IsDir():
				value.Kind = artifactEntryDirectory
				value.Mode = 0755
			case info.Mode().IsRegular():
				value.Kind = artifactEntryRegular
				value.Mode = 0644
				if info.Mode().Perm()&0111 != 0 {
					value.Mode = 0755
				}
				value.SizeBytes = info.Size()
				file, err := os.Open(name)
				if err != nil {
					return err
				}
				value.Content = file
				continued := yield(value, nil)
				closeErr := file.Close()
				if closeErr != nil {
					return closeErr
				}
				if !continued {
					return filepath.SkipAll
				}
				return nil
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(name)
				if err != nil {
					return err
				}
				value.Kind = artifactEntrySymlink
				value.Mode = 0777
				value.LinkTarget = filepath.ToSlash(target)
			default:
				return fmt.Errorf("Platform tree path %q has unsupported file type", relative)
			}
			if !yield(value, nil) {
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil && !errors.Is(err, filepath.SkipAll) {
			yield(treeEntry{}, err)
		}
	}
}

func writePlatformDocuments(
	ctx context.Context,
	root string,
	descriptor any,
	integrity PlatformIntegrity,
	conformance PlatformConformance,
	evidence platformEvidenceSet,
) error {
	helmr := filepath.Join(root, "helmr")
	upstream := filepath.Join(helmr, "upstream")
	if err := os.MkdirAll(upstream, 0755); err != nil {
		return fmt.Errorf("create Platform metadata tree: %w", err)
	}
	for name, raw := range evidence.documents {
		if strings.Contains(name, "/") || filepath.Base(name) != name || name == "" {
			return fmt.Errorf("Platform evidence name %q is invalid", name)
		}
		if err := os.WriteFile(filepath.Join(upstream, name), raw, 0644); err != nil {
			return fmt.Errorf("write Platform evidence %q: %w", name, err)
		}
	}
	if evidence.source != nil {
		if _, err := evidence.source.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind Platform source evidence: %w", err)
		}
		output, err := os.OpenFile(
			filepath.Join(upstream, "source"),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0644,
		)
		if err != nil {
			return fmt.Errorf("create Platform source evidence: %w", err)
		}
		written, copyErr := copyExact(
			ctx,
			output,
			evidence.source.file,
			evidence.source.source.SizeBytes,
		)
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil ||
			written != evidence.source.source.SizeBytes {
			return errors.Join(
				copyErr,
				closeErr,
				errors.New("Platform source evidence size changed"),
			)
		}
	}
	documents := []struct {
		name  string
		value any
	}{
		{PlatformDescriptorPath, descriptor},
		{PlatformIntegrityPath, integrity},
		{PlatformConformancePath, conformance},
	}
	for _, document := range documents {
		raw, err := CanonicalPlatformDocument(document.value)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(document.name)), raw, 0644); err != nil {
			return fmt.Errorf("write %s: %w", document.name, err)
		}
	}
	return normalizePlatformTree(root)
}

func normalizePlatformTree(root string) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Chmod(name, 0755)
		case info.Mode().IsRegular():
			mode := os.FileMode(0644)
			if info.Mode().Perm()&0111 != 0 {
				mode = 0755
			}
			return os.Chmod(name, mode)
		case info.Mode()&os.ModeSymlink != 0:
			return nil
		default:
			return fmt.Errorf("Platform tree contains unsupported path %q", name)
		}
	})
}

func copyExact(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	limit int64,
) (int64, error) {
	written, err := io.Copy(destination, io.LimitReader(source, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, errCopyExceedsLimit
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	return written, nil
}
