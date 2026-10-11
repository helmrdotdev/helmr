//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/vm"
)

// RemoveStoppedCgroup requires the exact retained owner marker. Kernel removal
// refuses live tasks and child groups; failure preserves that recovery evidence.
// This never stops unidentified tasks or removes the shared jailer parent.
func RemoveStoppedCgroup(stateDir, version string, owner vm.Owner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	state := filepath.Join(stateDir, owner.ID)
	info, err := os.Lstat(state)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("cgroup recovery owner root is not a direct directory")
	}
	marker, err := os.Lstat(filepath.Join(state, "owner"))
	if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return errors.New("cgroup recovery owner marker is not a regular file")
	}
	if err := validateOwnerMarker(state, owner); err != nil {
		return err
	}
	path, err := runtimeCgroupPath(version, owner.ID)
	if err != nil {
		return err
	}
	return removeRuntimeCgroup(path)
}

func runtimeCgroupPath(version, ownerID string) (string, error) {
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("inventory Firecracker cgroup mounts: %w", err)
	}
	return jailerCgroupPath(string(mounts), version, ownerID)
}

// The SDK supplies only cpuset properties and no parent override. The jailer
// uses the first matching mount and the executable name as its shared parent.
func jailerCgroupPath(mounts, version, ownerID string) (string, error) {
	if version == "" {
		version = DefaultCgroupVersion
	}
	for line := range strings.SplitSeq(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		match := version == "2" && fields[2] == "cgroup2"
		if version == "1" && fields[2] == "cgroup" {
			for option := range strings.SplitSeq(fields[3], ",") {
				if option == "cpuset" {
					match = true
				}
			}
		}
		if match {
			return filepath.Join(fields[1], "firecracker", ownerID), nil
		}
	}
	return "", fmt.Errorf("firecracker cgroup version %q mount not found", version)
}

func removeRuntimeCgroup(path string) error {
	if err := syscall.Rmdir(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("remove exact Firecracker cgroup: %w", err)
	}
	return nil
}
