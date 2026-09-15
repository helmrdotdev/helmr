//go:build linux || darwin

package buildcontext

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const nonblockFlag = unix.O_NONBLOCK

func normalizeEntry(root *os.Root, name string, info os.FileInfo) error {
	parent, base, err := entryParent(root, name)
	if err != nil {
		return err
	}
	defer parent.Close()
	if info == nil || info.Mode()&os.ModeSymlink == 0 {
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Chmod(normalizedMode(info)), file.Close()); err != nil {
			return err
		}
	}
	times := []unix.Timespec{unix.NsecToTimespec(captureEpoch.UnixNano()), unix.NsecToTimespec(captureEpoch.UnixNano())}
	return unix.UtimesNanoAt(int(parent.Fd()), base, times, unix.AT_SYMLINK_NOFOLLOW)
}
