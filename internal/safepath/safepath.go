// Package safepath contains small path-confinement primitives used by callers
// that keep their own trust-boundary policy.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type CleanOptions struct {
	AllowDot bool
}

func CleanSlash(raw string, options CleanOptions) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("path %q contains NUL", raw)
	}
	if raw == "" || path.IsAbs(raw) || filepath.IsAbs(raw) {
		return "", fmt.Errorf("unsafe path %q", raw)
	}
	slashed := filepath.ToSlash(raw)
	for part := range strings.SplitSeq(slashed, "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe path %q", raw)
		}
	}
	clean := path.Clean(slashed)
	if clean == "." && options.AllowDot {
		return clean, nil
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe path %q", raw)
	}
	return clean, nil
}

func CleanLocal(raw string, options CleanOptions) (string, error) {
	clean, err := CleanSlash(raw, options)
	if err != nil {
		return "", err
	}
	return filepath.FromSlash(clean), nil
}

func JoinSlash(root, relative string) (string, error) {
	clean := filepath.Clean(string(filepath.Separator) + filepath.FromSlash(relative))
	if clean == string(filepath.Separator) {
		return root, nil
	}
	target := filepath.Join(root, strings.TrimPrefix(clean, string(filepath.Separator)))
	contained, err := Contains(root, target)
	if err != nil {
		return "", err
	}
	if !contained {
		return "", fmt.Errorf("path escapes root: %s", relative)
	}
	return target, nil
}

func Contains(root, target string) (bool, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, nil
	}
	return true, nil
}

func MkdirAllNoSymlink(root, relative string, mode os.FileMode) error {
	if relative == "" || relative == "." {
		return nil
	}
	clean, err := CleanSlash(relative, CleanOptions{})
	if err != nil {
		return err
	}
	current := root
	for part := range strings.SplitSeq(clean, "/") {
		if part == "" {
			continue
		}
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
			return fmt.Errorf("unsafe path parent %q", current)
		}
	}
	return nil
}
