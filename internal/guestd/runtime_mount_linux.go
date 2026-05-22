//go:build linux

package guestd

import (
	"fmt"
	"path/filepath"
)

func mountImageRuntimeFilesystems(imageRoot string) (func(), error) {
	for _, rel := range []string{"proc", "dev"} {
		if err := mkdirAllNoSymlink(imageRoot, rel, 0o755); err != nil {
			return func() {}, err
		}
		if _, err := safeJoin(imageRoot, filepath.ToSlash(rel)); err != nil {
			return func() {}, err
		}
	}
	return func() {}, nil
}

func imageRuntimeMountTarget(imageRoot, rel string) (string, error) {
	if err := mkdirAllNoSymlink(imageRoot, rel, 0o755); err != nil {
		return "", err
	}
	return safeJoin(imageRoot, filepath.ToSlash(rel))
}

func imageRuntimeDeviceTarget(imageRoot, name string) (string, error) {
	if err := mkdirAllNoSymlink(imageRoot, "dev", 0o755); err != nil {
		return "", err
	}
	target, err := safeJoin(imageRoot, filepath.ToSlash(filepath.Join("dev", name)))
	if err != nil {
		return "", err
	}
	parent, err := safeJoin(imageRoot, "dev")
	if err != nil {
		return "", err
	}
	if filepath.Dir(target) != parent {
		return "", fmt.Errorf("device path escapes /dev: %s", name)
	}
	return target, nil
}
