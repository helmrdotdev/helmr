//go:build linux

package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/nbd"
)

func TestDeviceFailedClaimJoinsExportBeforeClosingGeneration(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	disk, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	arena, err := os.MkdirTemp("", "device-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(arena)
	device, err := AttachDevice(t.Context(), disk, nbd.Config{Helper: "/missing-nbd-helper", Arena: arena, Socket: filepath.Join(arena, "nbd"), Size: cfg.Base.LogicalBytes, Devices: []string{"/dev/nbd15"}})
	if err == nil || device == nil {
		t.Fatal("expected attributable failed attachment")
	}
	if err := device.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := device.Wait(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("export not canceled/joined: %v", err)
	}
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Lstat(filepath.Join(arena, "config.json")); err != nil {
		t.Fatal("recovery evidence removed", err)
	}
}

// Explicit disposable-kernel opt-in only; ordinary unit runs never claim devices.
func TestDeviceOwnedKernelLifecycle(t *testing.T) {
	if os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") != "1" {
		t.Skip("requires disposable NBD host")
	}
	helper := os.Getenv("HELMR_NBD_TEST_HELPER")
	if !filepath.IsAbs(helper) {
		t.Fatal("absolute helper required")
	}
	cfg, _ := localGenerationFixture(t)
	disk, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	arena, err := os.MkdirTemp("", "device-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retain on uncertainty:", arena)
	device, err := AttachDevice(t.Context(), disk, nbd.Config{Helper: helper, Arena: arena, Socket: filepath.Join(arena, "nbd"), Size: cfg.Base.LogicalBytes, Devices: []string{"/dev/nbd15", "/dev/nbd14"}})
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	if err := device.BindConsumer(exited); err != nil {
		t.Fatal(err)
	}
	path, err := device.LinkInto(t.Context(), t.TempDir(), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xb3}, 4096)
	if _, err := file.WriteAt(data, 4096); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	capture, err := device.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if capture.Root() == cfg.Base {
		t.Fatalf("flush: %v", err)
	}
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = device.Close(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live consumer release: %v", err)
	}
	got := make([]byte, len(data))
	if _, err := file.ReadAt(got, 4096); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("unproven release stopped live export: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	close(exited)
	if err := device.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := device.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ReadAt(t.Context(), got, 4096); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reopen: %v", err)
	}
	if err := os.RemoveAll(arena); err != nil {
		t.Fatal(err)
	}
}
