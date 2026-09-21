//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

func validateComputerDisk(disk *vm.RuntimeComputer) error {
	if disk == nil || disk.File == nil || disk.Path != "" || disk.SizeBytes <= 0 || disk.SizeBytes%4096 != 0 {
		return errors.New("computer working disk is incomplete")
	}
	if err := ids.Validate(disk.VersionID); err != nil {
		return err
	}
	info, err := disk.File.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != disk.SizeBytes {
		return errors.New("computer working disk size or file type changed")
	}
	return nil
}

// Link the exact host-owned inode, not a caller-controlled pathname or a shared
// cache object. The source is a fresh authenticated working copy for this runtime.
func attachComputerDisk(ctx context.Context, disk *vm.RuntimeComputer, directory string, uid, gid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateComputerDisk(disk); err != nil {
		return "", err
	}
	if err := disk.File.Chown(uid, gid); err != nil {
		return "", err
	}
	if err := disk.File.Chmod(0600); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "computer.ext4")
	if err := unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", disk.File.Fd()), unix.AT_FDCWD, path, unix.AT_SYMLINK_FOLLOW); err != nil {
		return "", fmt.Errorf("attach computer disk: %w", err)
	}
	return path, nil
}

func runtimeDrivesWithComputer(root, scratch, substrate, computer string, drives []vm.ReadOnlyDrive, paths map[string]string) []models.Drive {
	backing := substrate
	if computer != "" {
		backing = computer
	}
	result := runtimeDrivesWithReadOnlyPaths(root, scratch, backing, drives, paths)
	if computer != "" {
		result[2].DriveID = firecracker.String("computer")
		result[2].IsReadOnly = firecracker.Bool(false)
	}
	return result
}
