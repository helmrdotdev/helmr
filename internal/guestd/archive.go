package guestd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

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
