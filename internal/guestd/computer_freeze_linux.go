//go:build linux && computerproof

package guestd

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Linux UAPI _IOWR('X', 119/120, int), identical on supported amd64/arm64.
const computerFreezeIOCTL = 0xc0045877
const computerThawIOCTL = 0xc0045878

// computerFilesystem pins a verified mount independently of later path changes.
// Its owner must remain outside the customer filesystem and process scopes.
// Loss of this owner requires host-side VM termination, not automatic thaw.
type computerFilesystem struct {
	file   *os.File
	frozen bool
}

// device is the supervisor-owned block device used to mount root. The mount
// namespace and its ancestors must be supervisor-owned and quiescent during open.
// controlRoot pins the supervisor scratch filesystem, not merely the boot root.
func openComputerFilesystem(root string, device, controlRoot *os.File) (*computerFilesystem, error) {
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
	fail := func(err error) (*computerFilesystem, error) { _ = file.Close(); return nil, err }
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
	return &computerFilesystem{file: file}, nil
}

// freeze requires all customer scopes to be frozen. FIFREEZE flushes dirty pages
// and journal state before completing; it is not context-interruptible I/O.
func (f *computerFilesystem) freeze() error {
	if f.frozen {
		return fmt.Errorf("computer filesystem already frozen")
	}
	if err := unix.IoctlSetInt(int(f.file.Fd()), computerFreezeIOCTL, 0); err != nil {
		return fmt.Errorf("freeze computer filesystem: %w", err)
	}
	f.frozen = true
	return nil
}

// The caller must reconcile current authority before thawing any customer scope.
func (f *computerFilesystem) thaw() error {
	if !f.frozen {
		return nil
	}
	if err := unix.IoctlSetInt(int(f.file.Fd()), computerThawIOCTL, 0); err != nil {
		return fmt.Errorf("thaw computer filesystem: %w", err)
	}
	f.frozen = false
	return nil
}

func (f *computerFilesystem) close() error {
	if f.frozen {
		return fmt.Errorf("computer filesystem remains frozen; recovery owner required")
	}
	return f.file.Close()
}
