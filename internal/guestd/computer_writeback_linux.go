//go:build linux

package guestd

import (
	"golang.org/x/sys/unix"
	"os"
)

// Linux UAPI _IOWR('X', 119, int), identical on supported amd64/arm64.
const computerFreezeIOCTL = 0xc0045877

func openComputerWriteback() (*computerWriteback, error) {
	root := os.Getenv("HELMR_GUESTD_COMPUTER_ROOT")
	if root == "" {
		return nil, nil
	}
	device, err := os.Open("/dev/vdc")
	if err != nil {
		return nil, err
	}
	defer device.Close()
	control, err := os.Open("/var/lib/helmr")
	if err != nil {
		return nil, err
	}
	defer control.Close()
	file, err := openComputerFilesystemHandle(root, device, control)
	if err != nil {
		return nil, err
	}
	return &computerWriteback{
		syncFilesystem:   func() error { return unix.Syncfs(int(file.Fd())) },
		freezeFilesystem: func() error { return unix.IoctlSetInt(int(file.Fd()), computerFreezeIOCTL, 0) },
		closeFilesystem:  file.Close,
	}, nil
}
