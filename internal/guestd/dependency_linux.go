//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"golang.org/x/sys/unix"
)

type linuxStagedDependencyComponents struct {
	mounts []string
	root   string
}

func stageDependencyComponents(
	ctx context.Context,
	request deployment.ManagerRequest,
) (_ stagedDependencyComponents, retErr error) {
	components := dependencyComponents(request)
	if err := validateDependencyDevices(components); err != nil {
		return nil, err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("make dependency mount namespace private: %w", err)
	}
	root, err := mkdirGuestdTemp("helmr-dependency-*")
	if err != nil {
		return nil, fmt.Errorf("create dependency staging root: %w", err)
	}
	staged := &linuxStagedDependencyComponents{root: root}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, staged.Close())
		}
	}()
	for _, component := range components {
		if err := verifyDependencyDevice(ctx, component); err != nil {
			return nil, err
		}
		target := filepath.Join(root, component.name)
		if err := os.Mkdir(target, 0o700); err != nil {
			return nil, fmt.Errorf("create dependency mount %q: %w", component.name, err)
		}
		flags := uintptr(unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOSUID)
		if component.noexec {
			flags |= unix.MS_NOEXEC
		}
		if err := unix.Mount(component.device, target, "squashfs", flags, ""); err != nil {
			return nil, fmt.Errorf("mount dependency component %q: %w", component.name, err)
		}
		staged.mounts = append(staged.mounts, target)
	}
	return staged, nil
}

func validateDependencyDevices(components []dependencyComponent) error {
	expected := []string{"vda", "vdb"}
	for _, component := range components {
		expected = append(expected, filepath.Base(component.device))
	}
	slices.Sort(expected)
	devices, err := filepath.Glob("/sys/class/block/vd*")
	if err != nil {
		return fmt.Errorf("enumerate dependency block devices: %w", err)
	}
	actual := make([]string, 0, len(devices))
	for _, device := range devices {
		actual = append(actual, filepath.Base(device))
	}
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return fmt.Errorf(
			"dependency block devices = %v, want %v",
			actual,
			expected,
		)
	}
	return nil
}

func verifyDependencyDevice(ctx context.Context, component dependencyComponent) error {
	file, err := os.Open(component.device)
	if err != nil {
		return fmt.Errorf("open dependency component %q: %w", component.name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat dependency component %q: %w", component.name, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("dependency component %q is not a block device", component.name)
	}
	readOnly, err := os.ReadFile(
		filepath.Join("/sys/class/block", filepath.Base(component.device), "ro"),
	)
	if err != nil {
		return fmt.Errorf("read dependency component %q mode: %w", component.name, err)
	}
	if strings.TrimSpace(string(readOnly)) != "1" {
		return fmt.Errorf("dependency component %q is not read-only", component.name)
	}
	size, err := unix.IoctlGetInt(int(file.Fd()), unix.BLKGETSIZE64)
	if err != nil {
		return fmt.Errorf("read dependency component %q size: %w", component.name, err)
	}
	if int64(size) != component.artifact.SizeBytes {
		return fmt.Errorf(
			"dependency component %q size = %d, want %d",
			component.name,
			size,
			component.artifact.SizeBytes,
		)
	}
	if err := verifyDependencyContent(
		ctx,
		file,
		component.artifact.SizeBytes,
		component.artifact.Digest,
	); err != nil {
		return fmt.Errorf("verify dependency component %q: %w", component.name, err)
	}
	if err := deployment.VerifySquashFSPhysical(
		ctx,
		file,
		component.artifact.SizeBytes,
	); err != nil {
		return fmt.Errorf(
			"verify dependency component %q filesystem: %w",
			component.name,
			err,
		)
	}
	return nil
}

func (staged *linuxStagedDependencyComponents) Close() error {
	if staged == nil {
		return nil
	}
	var problems []error
	remaining := make([]string, 0, len(staged.mounts))
	for index := len(staged.mounts) - 1; index >= 0; index-- {
		target := staged.mounts[index]
		if err := unix.Unmount(target, 0); err != nil {
			problems = append(problems, fmt.Errorf("unmount dependency component %q: %w", target, err))
			remaining = append(remaining, target)
		}
	}
	slices.Reverse(remaining)
	staged.mounts = remaining
	if len(remaining) == 0 && staged.root != "" {
		if err := os.RemoveAll(staged.root); err != nil {
			problems = append(problems, fmt.Errorf("remove dependency staging root: %w", err))
		} else {
			staged.root = ""
		}
	}
	return errors.Join(problems...)
}
