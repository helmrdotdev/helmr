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
)

func extractTar(r io.Reader, dst string) error {
	return extractTarWithLimits(r, dst, tarExtractLimits{})
}

type tarExtractLimits struct {
	MaxBytes   int64
	MaxEntries int
}

func extractTarWithLimits(r io.Reader, dst string, limits tarExtractLimits) error {
	reader := tar.NewReader(r)
	var extractedBytes int64
	var extractedEntries int
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if tarEntryIsRootDir(header) {
			continue
		}
		extractedEntries++
		if limits.MaxEntries > 0 && extractedEntries > limits.MaxEntries {
			return fmt.Errorf("tar archive contains too many entries")
		}
		relative, err := tarEntryPath(header.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllNoSymlink(dst, relative, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := validateTarEntrySize(header, &extractedBytes, limits.MaxBytes); err != nil {
				return err
			}
			parent := filepath.ToSlash(filepath.Dir(relative))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(dst, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(relative, header.Linkname); err != nil {
				return err
			}
			parent := filepath.ToSlash(filepath.Dir(relative))
			if parent == "." {
				parent = ""
			}
			if err := mkdirAllNoSymlink(dst, parent, 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			return fmt.Errorf("unsupported tar hardlink entry %q", header.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("unsupported tar device entry %q type %d", header.Name, header.Typeflag)
		default:
			return fmt.Errorf("unsupported tar entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

func validateTarEntrySize(header *tar.Header, extractedBytes *int64, maxBytes int64) error {
	if hasSparseTarMetadata(header) {
		return fmt.Errorf("unsupported sparse tar entry %q", header.Name)
	}
	if header.Size < 0 {
		return fmt.Errorf("tar entry %q has invalid size", header.Name)
	}
	if maxBytes <= 0 {
		*extractedBytes += header.Size
		return nil
	}
	if header.Size > maxBytes {
		return fmt.Errorf("tar entry %q exceeds extracted size limit", header.Name)
	}
	if *extractedBytes > maxBytes-header.Size {
		return fmt.Errorf("tar archive exceeds extracted size limit")
	}
	*extractedBytes += header.Size
	return nil
}

func hasSparseTarMetadata(header *tar.Header) bool {
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") || strings.HasPrefix(key, "SCHILY.realsize") {
			return true
		}
	}
	return false
}

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
	if filepath.IsAbs(name) || strings.HasPrefix(filepath.FromSlash(name), string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe tar path %q", name)
		}
	}
	relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
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
	current := root
	for _, part := range strings.Split(clean, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe tar parent %q", current)
		}
	}
	return nil
}

func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(name))
	if clean == "/" {
		return root, nil
	}
	target := filepath.Join(root, strings.TrimPrefix(clean, string(filepath.Separator)))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar path escapes destination: %s", name)
	}
	return target, nil
}

func copyTree(sourceRoot, destinationRoot string) error {
	return copyTreeSkipping(sourceRoot, destinationRoot, nil)
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
		target, err := safeJoin(destinationRoot, filepath.ToSlash(rel))
		if err != nil {
			return err
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
