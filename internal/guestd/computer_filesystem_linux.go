//go:build linux

package guestd

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// device is the supervisor-owned block device used to mount root. The mount
// namespace and its ancestors must be supervisor-owned and quiescent during open.
// controlRoot pins the supervisor scratch filesystem, not merely the boot root.
func openComputerFilesystemHandle(root string, device, controlRoot *os.File) (*os.File, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), root)
	fail := func(err error) (*os.File, error) { _ = file.Close(); return nil, err }
	var disk, mounted, control unix.Stat_t
	if err := unix.Fstat(int(device.Fd()), &disk); err != nil {
		return fail(err)
	}
	if disk.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fail(fmt.Errorf("computer device is not a block device"))
	}
	if err := unix.Fstat(fd, &mounted); err != nil {
		return fail(err)
	}
	if mounted.Dev != disk.Rdev {
		return fail(fmt.Errorf("computer mount does not match its device"))
	}
	if err := unix.Fstat(int(controlRoot.Fd()), &control); err != nil {
		return fail(err)
	}
	if mounted.Dev == control.Dev {
		return fail(fmt.Errorf("computer mount shares the control filesystem"))
	}
	var mount, parent unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &mount); err != nil {
		return fail(err)
	}
	if err := unix.Statx(unix.AT_FDCWD, filepath.Dir(root), unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &parent); err != nil {
		return fail(err)
	}
	if mount.Mask&unix.STATX_MNT_ID == 0 || parent.Mask&unix.STATX_MNT_ID == 0 || mount.Mnt_id == parent.Mnt_id {
		return fail(fmt.Errorf("computer root is not a distinct mount"))
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return fail(err)
	}
	if filesystem.Type != unix.EXT4_SUPER_MAGIC {
		return fail(fmt.Errorf("computer filesystem type %#x is not ext4", filesystem.Type))
	}
	return file, nil
}
