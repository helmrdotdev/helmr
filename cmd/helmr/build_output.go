package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// extractBuildOutput materializes a BuildKit filesystem export in a fresh root.
// Ownership and special permission bits from the build never apply to the host.
func extractBuildOutput(ctx context.Context, source io.Reader, destination string) error {
	if err := os.Mkdir(destination, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(buildOutputReader{ctx: ctx, source: source})
	seen := map[string]bool{}
	var directories []*tar.Header
	var links []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read build output archive: %w", err)
		}
		name := filepath.Clean(header.Name)
		if !filepath.IsLocal(header.Name) || slices.Contains(strings.Split(filepath.ToSlash(header.Name), "/"), "..") || seen[name] {
			return fmt.Errorf("invalid or repeated build output path %q", header.Name)
		}
		if header.Mode&07000 != 0 {
			return fmt.Errorf("unsupported build output permissions for %q", name)
		}
		header.Name = name
		seen[name] = true
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			info, err := root.Lstat(parent)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("build output parent %q is not a directory", parent)
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			info, err := root.Lstat(name)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("build output directory %q is not a directory", name)
			}
			directories = append(directories, header)
		case tar.TypeReg:
			file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			modeErr := file.Chmod(os.FileMode(header.Mode) & os.ModePerm)
			if err := errors.Join(copyErr, modeErr, file.Close()); err != nil {
				return err
			}
			if err := root.Chtimes(name, header.ModTime, header.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), header.Linkname)) {
				return fmt.Errorf("build output symlink %q escapes its root", name)
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return err
			}
			links = append(links, name)
		case tar.TypeLink:
			if !filepath.IsLocal(header.Linkname) {
				return fmt.Errorf("invalid build output hardlink %q", name)
			}
			info, err := root.Lstat(header.Linkname)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("build output hardlink %q does not target a regular file", name)
			}
			if err := root.Link(header.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported build output entry %q (type %d)", name, header.Typeflag)
		}
	}
	// Creation is confined by Root, but a stored symlink target also needs to
	// resolve within it, including ".." components after another link expands.
	for _, name := range links {
		if _, err := root.Stat(name); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("resolve build output symlink %q: %w", name, err)
		}
	}
	// Keep directories writable until their children have been extracted.
	slices.SortFunc(directories, func(a, b *tar.Header) int {
		return len(b.Name) - len(a.Name)
	})
	for _, header := range directories {
		if err := root.Chmod(header.Name, os.FileMode(header.Mode)&os.ModePerm); err != nil {
			return err
		}
		if err := root.Chtimes(header.Name, header.ModTime, header.ModTime); err != nil {
			return err
		}
	}
	return nil
}

type buildOutputReader struct {
	ctx    context.Context
	source io.Reader
}

func (r buildOutputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
