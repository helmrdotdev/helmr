package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (tree *BuildTree) MaterializeApplication(
	ctx context.Context,
	directory string,
) (_ string, cleanup func() error, returnErr error) {
	if ctx == nil {
		return "", nil, errors.New("build tree materialization context is nil")
	}
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return "", nil, errors.New("build tree is closed")
	}
	if directory == "" ||
		!filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return "", nil, errors.New("build tree materialization directory must be an absolute clean path")
	}
	root, err := os.MkdirTemp(directory, ".helmr-build-source-")
	if err != nil {
		return "", nil, fmt.Errorf("create build source: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", nil, errors.Join(err, os.Remove(root))
	}
	remove := func() error {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
		return os.RemoveAll(root)
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, remove())
		}
	}()
	for _, entry := range tree.inspected.ordered {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if entry.Path == "." {
			continue
		}
		if applicationViewReserved(entry.Path) {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(entry.Path))
		switch entry.Kind {
		case artifactEntryDirectory:
			if err := os.Mkdir(target, 0o700); err != nil {
				return "", nil, fmt.Errorf("create build source directory %q: %w", entry.Path, err)
			}
		case artifactEntryRegular:
			if err := materializeBuildFile(ctx, tree, entry, target); err != nil {
				return "", nil, err
			}
		case artifactEntrySymlink:
			if err := os.Symlink(entry.LinkTarget, target); err != nil {
				return "", nil, fmt.Errorf("create build source link %q: %w", entry.Path, err)
			}
		default:
			return "", nil, fmt.Errorf("build source path %q has unsupported type", entry.Path)
		}
	}
	for index := len(tree.inspected.ordered) - 1; index >= 0; index-- {
		entry := tree.inspected.ordered[index]
		if entry.Path == "." ||
			applicationViewReserved(entry.Path) ||
			entry.Kind == artifactEntrySymlink {
			continue
		}
		mode := os.FileMode(entry.Mode)
		if entry.Kind == artifactEntryDirectory {
			mode = 0o555
		} else {
			mode &^= 0o222
		}
		if err := os.Chmod(
			filepath.Join(root, filepath.FromSlash(entry.Path)),
			mode,
		); err != nil {
			return "", nil, fmt.Errorf("freeze build source path %q: %w", entry.Path, err)
		}
	}
	if err := os.Chmod(root, 0o555); err != nil {
		return "", nil, fmt.Errorf("freeze build source root: %w", err)
	}
	return root, remove, nil
}

func applicationViewReserved(name string) bool {
	return name == "helmr" || strings.HasPrefix(name, "helmr/") ||
		name == "node_modules" || strings.HasPrefix(name, "node_modules/")
}

func materializeBuildFile(
	ctx context.Context,
	tree *BuildTree,
	entry artifactEntry,
	target string,
) (returnErr error) {
	source, err := tree.inspected.reader.Open(ctx, entry.Path)
	if err != nil {
		return fmt.Errorf("open frozen build path %q: %w", entry.Path, err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()
	destination, err := os.OpenFile(
		target,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create build source file %q: %w", entry.Path, err)
	}
	written, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if written != entry.SizeBytes {
		return fmt.Errorf(
			"build source file %q size = %d, want %d",
			entry.Path,
			written,
			entry.SizeBytes,
		)
	}
	return nil
}
