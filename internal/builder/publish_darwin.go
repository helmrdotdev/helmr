//go:build darwin

package builder

import (
	"errors"

	"golang.org/x/sys/unix"
)

func publishBundleDirectory(source string, destination string) error {
	err := unix.RenameatxNp(
		unix.AT_FDCWD,
		source,
		unix.AT_FDCWD,
		destination,
		unix.RENAME_EXCL,
	)
	if errors.Is(err, unix.EEXIST) {
		return errors.New("bundle output directory already exists")
	}
	return err
}
