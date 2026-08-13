//go:build linux

package builder

import (
	"errors"

	"golang.org/x/sys/unix"
)

func publishBundleDirectory(source string, destination string) error {
	err := unix.Renameat2(
		unix.AT_FDCWD,
		source,
		unix.AT_FDCWD,
		destination,
		unix.RENAME_NOREPLACE,
	)
	if errors.Is(err, unix.EEXIST) {
		return errors.New("bundle output directory already exists")
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return errors.New("atomic no-replace bundle publication is unsupported")
	}
	return err
}
