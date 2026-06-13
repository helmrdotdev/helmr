package guestd

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

func tarEntryIsRootDir(header *tar.Header) bool {
	if header == nil || header.Typeflag != tar.TypeDir {
		return false
	}
	name := strings.TrimSpace(header.Name)
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(filepath.FromSlash(name), string(filepath.Separator)) {
		return false
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))) == "."
}

func tarEntryPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("tar path is empty")
	}
	relative, err := safepath.CleanSlash(name, safepath.CleanOptions{})
	if err != nil {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return relative, nil
}

func mkdirAllNoSymlink(root, relative string, mode os.FileMode) error {
	if relative == "" || relative == "." {
		return nil
	}
	clean, err := tarEntryPath(relative)
	if err != nil {
		return err
	}
	if err := safepath.MkdirAllNoSymlink(root, clean, mode); err != nil {
		return fmt.Errorf("unsafe tar parent: %w", err)
	}
	return nil
}

func copyTreeSkipping(sourceRoot, destinationRoot string, skip func(rel string, isDir bool) bool) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != nil && skip(filepath.ToSlash(rel), entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target, err := safepath.JoinSlash(destinationRoot, filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("tar path escapes destination: %s", filepath.ToSlash(rel))
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			parent := filepath.ToSlash(filepath.Dir(rel))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(destinationRoot, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			return mkdirAllNoSymlink(destinationRoot, filepath.ToSlash(rel), info.Mode()&0o777)
		case info.Mode().IsRegular():
			parent := filepath.ToSlash(filepath.Dir(rel))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(destinationRoot, parent, 0o755); err != nil {
				return err
			}
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				_ = source.Close()
				return err
			}
			destination, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, info.Mode()&0o777)
			if err != nil {
				_ = source.Close()
				return err
			}
			_, copyErr := io.Copy(destination, source)
			sourceCloseErr := source.Close()
			closeErr := destination.Close()
			if copyErr != nil {
				return copyErr
			}
			if sourceCloseErr != nil {
				return sourceCloseErr
			}
			return closeErr
		default:
			return nil
		}
	})
}
