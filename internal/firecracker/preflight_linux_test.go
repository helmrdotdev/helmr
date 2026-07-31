//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckCommandRequiresExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand("tool", path); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("error = %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand("tool", path); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightChecksContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Connector{cfg: (Config{}).WithDefaults()}).Preflight(ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckHardLinkLayoutRejectsSeparateBindMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bind-mount regression requires root")
	}
	source := t.TempDir()
	bind := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(bind, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(source, bind, "", unix.MS_BIND, ""); err != nil {
		if err == unix.EPERM {
			t.Skip("bind-mount regression requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	defer unix.Unmount(bind, 0)

	state := filepath.Join(source, "state")
	jailer := filepath.Join(source, "jailer")
	for _, path := range []string{state, jailer} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		KernelPath:          filepath.Join(bind, "vmlinuz"),
		InitramfsPath:       filepath.Join(bind, "initramfs"),
		RootfsPath:          filepath.Join(bind, "rootfs.ext4"),
		StateDir:            state,
		JailerChrootBaseDir: jailer,
	}
	for _, path := range []string{
		filepath.Join(source, "vmlinuz"),
		filepath.Join(source, "initramfs"),
		filepath.Join(source, "rootfs.ext4"),
	} {
		if err := os.WriteFile(path, []byte("artifact"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	err := checkHardLinkLayout(cfg)
	if err == nil || !strings.Contains(err.Error(), "hard-link layout") {
		t.Fatalf("error = %v", err)
	}
}
