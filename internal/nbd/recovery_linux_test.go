//go:build linux

package nbd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIdleDeviceInventory(t *testing.T) {
	for _, kind := range []string{"idle", "wrong-identity", "missing-identity", "pid", "pid-symlink", "nonzero", "invalid-size", "missing-size"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "missing-identity" {
				write("dev", "43:0\n")
			}
			if kind != "missing-size" {
				write("size", "0\n")
			}
			switch kind {
			case "wrong-identity":
				write("dev", "43:1")
			case "pid":
				write("pid", "0")
			case "pid-symlink":
				if err := os.Symlink("absent", filepath.Join(root, "pid")); err != nil {
					t.Fatal(err)
				}
			case "nonzero":
				write("size", "1")
			case "invalid-size":
				write("size", "unknown")
			}
			err := inspectIdleDevice(root, unix.Mkdev(43, 0))
			if (err == nil) != (kind == "idle") {
				t.Fatalf("unexpected inventory result: %v", err)
			}
		})
	}
}

func TestIdleDeviceProbeRejectsRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyIdleDevice(path); err == nil {
		t.Fatal("regular file accepted as device")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("probe mutated file: %q, %v", data, err)
	}
}

func TestIdleDeviceProbeRetainsCloseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := verifyIdleDeviceWithClose(path, func(fd int) error {
		return errors.Join(unix.Close(fd), unix.EIO)
	})
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("close failure was lost behind inspection failure: %v", err)
	}
}

func TestIdleDeviceKernelProbe(t *testing.T) {
	device := os.Getenv("HELMR_NBD_RECOVERY_DEVICE")
	if device == "" {
		t.Skip("requires an explicitly authorized idle dedicated Linux NBD device")
	}
	if err := VerifyIdleDevices([]string{device}); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	busyErr := VerifyIdleDevices([]string{device})
	closeErr := unix.Close(fd)
	if !errors.Is(busyErr, unix.EBUSY) || closeErr != nil {
		t.Fatalf("exclusive claim not respected: probe=%v close=%v", busyErr, closeErr)
	}
	err = verifyIdleDeviceWithClose(device, func(fd int) error {
		return errors.Join(unix.Close(fd), unix.EIO)
	})
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("close failure was accepted after idle inspection: %v", err)
	}
	if err := VerifyIdleDevices([]string{device}); err != nil {
		t.Fatalf("probe left device unavailable: %v", err)
	}
	// Observe again after the successful descriptor was closed, so a residual
	// configuration cannot be hidden by the probe's own open reference.
	var stat unix.Stat_t
	if err := unix.Lstat(device, &stat); err != nil {
		t.Fatal(err)
	}
	if err := inspectIdleDevice(filepath.Join("/sys/block", filepath.Base(device)), uint64(stat.Rdev)); err != nil {
		t.Fatalf("probe left driver state: %v", err)
	}
}
