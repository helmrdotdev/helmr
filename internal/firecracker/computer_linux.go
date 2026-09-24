//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

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
	size, err := computerBackingSize(disk.File, info)
	if err != nil {
		return err
	}
	if size != disk.SizeBytes {
		return errors.New("computer working disk size changed")
	}
	return nil
}

// Link the exact host-owned inode, not a caller-controlled pathname or a shared
// cache object or global device node. The Runtime owns this backing inode and,
// for block devices, its exclusive attachment and export through VMM exit.
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
		result[2].CacheType = firecracker.String(writableBlockCache)
	}
	return result
}

// A block node's stat size is zero; query the retained descriptor, never a device
// name. The Runtime owns the attachment for the complete VMM lifetime.
func computerBackingSize(file *os.File, info os.FileInfo) (int64, error) {
	if info.Mode().IsRegular() {
		return info.Size(), nil
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return 0, errors.New("computer backing must be a regular file or block device")
	}
	var size uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0, errno
	}
	if size == 0 || size > uint64(^uint64(0)>>1) {
		return 0, errors.New("invalid block device capacity")
	}
	return int64(size), nil
}

func validComputerBacking(info os.FileInfo) bool {
	return info.Mode().IsRegular() || info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0
}
