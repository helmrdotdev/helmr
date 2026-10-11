//go:build linux && computerproof

package guestd

import (
	"fmt"
	"os"

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

func openComputerFilesystem(root string, device, controlRoot *os.File) (*computerFilesystem, error) {
	file, err := openComputerFilesystemHandle(root, device, controlRoot)
	if err != nil {
		return nil, err
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
