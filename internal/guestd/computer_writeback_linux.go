//go:build linux

package guestd

import (
	"golang.org/x/sys/unix"
	"os"
)

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
		syncFilesystem:  func() error { return unix.Syncfs(int(file.Fd())) },
		closeFilesystem: file.Close,
	}, nil
}
