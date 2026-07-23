//go:build linux

package guestd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"golang.org/x/sys/unix"
)

type buildComponent struct {
	artifact deployment.ArtifactDescriptor
	device   string
	name     string
	noexec   bool
}

type stagedBuildComponents struct {
	mounts []string
	root   string
}

func stageBuildComponents(
	ctx context.Context,
	components []buildComponent,
) (_ *stagedBuildComponents, returnErr error) {
	if err := validateBuildDevices(components); err != nil {
		return nil, err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("make build mount namespace private: %w", err)
	}
	root, err := mkdirGuestdTemp("helmr-build-components-*")
	if err != nil {
		return nil, fmt.Errorf("create build component root: %w", err)
	}
	staged := &stagedBuildComponents{root: root}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, staged.Close())
		}
	}()
	for _, component := range components {
		if err := verifyBuildDevice(ctx, component); err != nil {
			return nil, err
		}
		target := filepath.Join(root, component.name)
		if err := os.Mkdir(target, 0o700); err != nil {
			return nil, fmt.Errorf("create build component mount %q: %w", component.name, err)
		}
		flags := uintptr(unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOSUID)
		if component.noexec {
			flags |= unix.MS_NOEXEC
		}
		if err := unix.Mount(component.device, target, "squashfs", flags, ""); err != nil {
			return nil, fmt.Errorf("mount build component %q: %w", component.name, err)
		}
		staged.mounts = append(staged.mounts, target)
	}
	return staged, nil
}

func validateBuildDevices(components []buildComponent) error {
	expected := []string{"vda", "vdb"}
	for index, component := range components {
		device := fmt.Sprintf("/dev/vd%c", 'c'+index)
		if component.device != device {
			return fmt.Errorf("build component %q device = %q, want %q", component.name, component.device, device)
		}
		expected = append(expected, filepath.Base(device))
	}
	slices.Sort(expected)
	devices, err := filepath.Glob("/sys/class/block/vd*")
	if err != nil {
		return fmt.Errorf("enumerate build block devices: %w", err)
	}
	actual := make([]string, 0, len(devices))
	for _, device := range devices {
		actual = append(actual, filepath.Base(device))
	}
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return fmt.Errorf("build block devices = %v, want %v", actual, expected)
	}
	return nil
}

func verifyBuildDevice(ctx context.Context, component buildComponent) error {
	file, err := os.Open(component.device)
	if err != nil {
		return fmt.Errorf("open build component %q: %w", component.name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat build component %q: %w", component.name, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("build component %q is not a block device", component.name)
	}
	readOnly, err := os.ReadFile(filepath.Join(
		"/sys/class/block",
		filepath.Base(component.device),
		"ro",
	))
	if err != nil {
		return fmt.Errorf("read build component %q mode: %w", component.name, err)
	}
	if strings.TrimSpace(string(readOnly)) != "1" {
		return fmt.Errorf("build component %q is not read-only", component.name)
	}
	size, err := unix.IoctlGetInt(int(file.Fd()), unix.BLKGETSIZE64)
	if err != nil {
		return fmt.Errorf("read build component %q size: %w", component.name, err)
	}
	if int64(size) != component.artifact.SizeBytes {
		return fmt.Errorf(
			"build component %q size = %d, want %d",
			component.name,
			size,
			component.artifact.SizeBytes,
		)
	}
	if err := verifyBuildComponentContent(
		ctx,
		file,
		component.artifact.SizeBytes,
		component.artifact.Digest,
	); err != nil {
		return fmt.Errorf("verify build component %q: %w", component.name, err)
	}
	if err := deployment.VerifySquashFSPhysical(
		ctx,
		file,
		component.artifact.SizeBytes,
	); err != nil {
		return fmt.Errorf("verify build component %q filesystem: %w", component.name, err)
	}
	return nil
}

func verifyBuildComponentContent(
	ctx context.Context,
	source *os.File,
	size int64,
	digest string,
) error {
	if ctx == nil {
		return errors.New("build component context is nil")
	}
	if source == nil {
		return errors.New("build component source is nil")
	}
	if size <= 0 {
		return fmt.Errorf("build component size = %d", size)
	}
	encoded, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(encoded) != sha256.Size*2 ||
		encoded != strings.ToLower(encoded) {
		return errors.New("build component digest is not a lowercase SHA-256 digest")
	}
	expected, err := hex.DecodeString(encoded)
	if err != nil {
		return errors.New("build component digest is not a lowercase SHA-256 digest")
	}
	hasher := sha256.New()
	buffer := make([]byte, 128<<10)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(len(buffer))
		if remaining < length {
			length = remaining
		}
		if _, err := io.ReadFull(source, buffer[:int(length)]); err != nil {
			return fmt.Errorf("read build component: %w", err)
		}
		_, _ = hasher.Write(buffer[:int(length)])
		remaining -= length
	}
	actual := hasher.Sum(nil)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return fmt.Errorf(
			"build component digest = sha256:%s, want %s",
			hex.EncodeToString(actual),
			digest,
		)
	}
	return nil
}

func (staged *stagedBuildComponents) Path(name string) (string, error) {
	if staged == nil || staged.root == "" {
		return "", errors.New("build components are not staged")
	}
	for _, mount := range staged.mounts {
		if filepath.Base(mount) == name {
			return mount, nil
		}
	}
	return "", fmt.Errorf("build component %q is not staged", name)
}

func (staged *stagedBuildComponents) Close() error {
	if staged == nil {
		return nil
	}
	var problems []error
	remaining := make([]string, 0, len(staged.mounts))
	for index := len(staged.mounts) - 1; index >= 0; index-- {
		target := staged.mounts[index]
		if err := unix.Unmount(target, 0); err != nil {
			problems = append(problems, fmt.Errorf("unmount build component %q: %w", target, err))
			remaining = append(remaining, target)
		}
	}
	slices.Reverse(remaining)
	staged.mounts = remaining
	if len(remaining) == 0 && staged.root != "" {
		if err := os.RemoveAll(staged.root); err != nil {
			problems = append(problems, fmt.Errorf("remove build component root: %w", err))
		} else {
			staged.root = ""
		}
	}
	return errors.Join(problems...)
}
