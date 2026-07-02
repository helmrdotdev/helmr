//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/oci"
)

const guestdSubstrateRootEnv = "HELMR_GUESTD_SUBSTRATE_ROOT"

func guestdSubstrateRoot() string {
	return strings.TrimSpace(os.Getenv(guestdSubstrateRootEnv))
}

func imageFromMountedSubstrate(r io.Reader, substrateRoot string) (ociImage, func(), error) {
	substrateRoot = filepath.Clean(strings.TrimSpace(substrateRoot))
	if substrateRoot == "" || substrateRoot == "." {
		return ociImage{}, func() {}, errors.New("runtime substrate root is required")
	}
	config, err := oci.ReadConfig(r)
	if err != nil {
		return ociImage{}, func() {}, err
	}
	imageRoot, cleanup, err := createOverlayImageRoot(substrateRoot)
	if err != nil {
		return ociImage{}, cleanup, err
	}
	return ociImage{RootfsDir: imageRoot, Config: config}, cleanup, nil
}

func createOverlayImageRoot(substrateRoot string) (string, func(), error) {
	root, err := mkdirGuestdTemp("helmr-image-overlay-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create image overlay root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	merged := filepath.Join(root, "merged")
	for _, dir := range []string{upper, work, merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("create image overlay directory: %w", err)
		}
	}
	data, err := overlayMountData(substrateRoot, upper, work)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := syscall.Mount("overlay", merged, "overlay", 0, data); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("mount image overlay: %w", err)
	}
	return merged, func() {
		_ = syscall.Unmount(merged, syscall.MNT_DETACH)
		_ = os.RemoveAll(root)
	}, nil
}

func overlayMountData(lower string, upper string, work string) (string, error) {
	for label, path := range map[string]string{"lowerdir": lower, "upperdir": upper, "workdir": work} {
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("overlay %s is required", label)
		}
		if strings.Contains(path, ",") {
			return "", fmt.Errorf("overlay %s must not contain comma: %q", label, path)
		}
	}
	return "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work, nil
}
