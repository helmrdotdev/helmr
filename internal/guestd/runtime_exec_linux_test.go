//go:build linux

package guestd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestImageAdapterCommandUsesNamespaceInit(t *testing.T) {
	cmd, err := adapterCommand(context.Background(), "/opt/helmr/lib/ld-linux.so", []string{"/opt/helmr/bin/node", "/opt/helmr/adapter/main.js"}, "/workspace", []string{"A=B"}, "/image", &resolvedRuntimeUser{UID: 1001, GID: 1002}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/proc/self/exe" {
		t.Fatalf("path = %q", cmd.Path)
	}
	if len(cmd.Args) < 8 || cmd.Args[1] != imageAdapterInitArg {
		t.Fatalf("args = %#v", cmd.Args)
	}
	if cmd.Args[2] != "/image" || cmd.Args[3] != "/workspace" || cmd.Args[4] != "1001" || cmd.Args[5] != "1002" || cmd.Args[6] != "/opt/helmr/lib/ld-linux.so" {
		t.Fatalf("init args = %#v", cmd.Args)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if cmd.SysProcAttr.Chroot != "" {
		t.Fatalf("parent command chroot = %q", cmd.SysProcAttr.Chroot)
	}
	if cmd.SysProcAttr.Credential != nil {
		t.Fatalf("parent command credential = %+v", cmd.SysProcAttr.Credential)
	}
	want := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWPID)
	if cmd.SysProcAttr.Cloneflags&want != want {
		t.Fatalf("clone flags = %#x, want %#x", cmd.SysProcAttr.Cloneflags, want)
	}
}

func TestMountImageRuntimeFilesystemsDoesNotExposeHostProcOrDev(t *testing.T) {
	root := t.TempDir()
	cleanup, err := mountImageRuntimeFilesystems(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(root, "proc", "self")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("/proc/self exposed in image root, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dev", "null")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("/dev/null exposed before private dev setup, stat err = %v", err)
	}
}

func TestMountImageRuntimeFilesystemsRejectsSymlinkedMountPoints(t *testing.T) {
	for _, name := range []string{"proc", "dev"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink(t.TempDir(), filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := mountImageRuntimeFilesystems(root); err == nil {
				t.Fatal("expected symlinked runtime path rejection")
			}
		})
	}
}
