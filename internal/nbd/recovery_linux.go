//go:build linux

package nbd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// VerifyIdleDevices observes dedicated devices without disconnecting or clearing
// them. A successful probe is not a reservation or proof of consumer cessation.
// Exclusive open can wait for an in-progress kernel claim.
func VerifyIdleDevices(devices []string) error {
	if len(devices) == 0 {
		return errors.New("no dedicated computer devices configured")
	}
	for _, device := range devices {
		if !validDevice(device) {
			return fmt.Errorf("invalid dedicated computer device %q", device)
		}
		if err := verifyIdleDevice(device); err != nil {
			return fmt.Errorf("computer device %s requires reconciliation: %w", device, err)
		}
	}
	return nil
}

func verifyIdleDevice(device string) error {
	return verifyIdleDeviceWithClose(device, unix.Close)
}

// Keep close failure observable in tests without replacing the kernel probe.
func verifyIdleDeviceWithClose(device string, closeFD func(int) error) (err error) {
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("acquire exclusive observation: %w", err)
	}
	defer func() { err = errors.Join(err, closeFD(fd)) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		return errors.New("dedicated device is not a block device")
	}
	return inspectIdleDevice(filepath.Join("/sys/block", filepath.Base(device)), uint64(stat.Rdev))
}

func inspectIdleDevice(root string, rdev uint64) error {
	identity, err := os.ReadFile(filepath.Join(root, "dev"))
	if err != nil {
		return err
	}
	expected := fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev))
	if strings.TrimSpace(string(identity)) != expected {
		return errors.New("sysfs device identity does not match claimed descriptor")
	}
	if _, err := os.Lstat(filepath.Join(root, "pid")); err == nil {
		return errors.New("device still has a driver task")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	size, err := os.ReadFile(filepath.Join(root, "size"))
	if err != nil {
		return err
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || sectors != 0 {
		return errors.New("device size is nonzero or unreadable")
	}
	return nil
}
