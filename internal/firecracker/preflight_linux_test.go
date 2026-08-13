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
	err := (&Connector{cfg: (Config{}).WithDefaults()}).preflight(ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v", err)
	}
}

func TestJailerDeviceMountRejectsNodev(t *testing.T) {
	path := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Flags&unix.ST_NODEV != 0 {
		t.Skip("test filesystem is already mounted nodev")
	}
	if err := checkJailerDeviceMount(path); err != nil {
		t.Fatal(err)
	}
	if err := validateJailerDeviceMountFlags(unix.ST_NODEV); err == nil || !strings.Contains(err.Error(), "forbids device nodes") {
		t.Fatalf("nodev error = %v", err)
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
		RootfsPath:          filepath.Join(bind, "rootfs.squashfs"),
		StateDir:            state,
		JailerChrootBaseDir: jailer,
	}
	for _, path := range []string{
		filepath.Join(source, "vmlinuz"),
		filepath.Join(source, "initramfs"),
		filepath.Join(source, "rootfs.squashfs"),
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

func TestSecureDirectoryRejectsSymlinkAndBroadWritePermission(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkSecureDirectory("state", directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o720); err != nil {
		t.Fatal(err)
	}
	if err := checkSecureDirectory("state", directory); err == nil || !strings.Contains(err.Error(), "group or world") {
		t.Fatalf("broad write permission error = %v", err)
	}
	link := filepath.Join(root, "state-link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if err := checkSecureDirectory("state", link); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestResolvedStateLayoutRejectsPhysicalAlias(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jailerAlias := filepath.Join(root, "jailer")
	if err := os.Symlink(stateDir, jailerAlias); err != nil {
		t.Fatal(err)
	}
	err := checkResolvedStateLayout(Config{StateDir: stateDir, JailerChrootBaseDir: jailerAlias})
	if err == nil || !strings.Contains(err.Error(), "must be disjoint") {
		t.Fatalf("error = %v", err)
	}
}
