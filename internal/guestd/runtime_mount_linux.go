//go:build linux

package guestd

import (
	"fmt"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

func mountImageRuntimeFilesystems(imageRoot string) (func(), error) {
	for _, rel := range []string{"proc", "dev"} {
		if err := mkdirAllNoSymlink(imageRoot, rel, 0o755); err != nil {
			return func() {}, err
		}
		if _, err := safepath.JoinSlash(imageRoot, filepath.ToSlash(rel)); err != nil {
			return func() {}, err
		}
	}
	return func() {}, nil
}

func imageRuntimeMountTarget(imageRoot, rel string) (string, error) {
	if err := mkdirAllNoSymlink(imageRoot, rel, 0o755); err != nil {
		return "", err
	}
	return safepath.JoinSlash(imageRoot, filepath.ToSlash(rel))
}

func imageRuntimeDeviceTarget(imageRoot, name string) (string, error) {
	if err := mkdirAllNoSymlink(imageRoot, "dev", 0o755); err != nil {
		return "", err
	}
	target, err := safepath.JoinSlash(imageRoot, filepath.ToSlash(filepath.Join("dev", name)))
	if err != nil {
		return "", err
	}
	parent, err := safepath.JoinSlash(imageRoot, "dev")
	if err != nil {
		return "", err
	}
	if filepath.Dir(target) != parent {
		return "", fmt.Errorf("device path escapes /dev: %s", name)
	}
	return target, nil
}
