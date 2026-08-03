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

func TestImageCommandUsesNamespaceInit(t *testing.T) {
	leaf, err := programCgroupLeafName("run-1", 1, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := imageCommand(context.Background(), "/usr/bin/node", []string{"/opt/helmr/program/helmr/entry.mjs"}, "/workspace", []string{"A=B"}, "/image", &resolvedRuntimeUser{UID: 1001, GID: 1002}, imageCommandOptions{ManagedProgram: true, CgroupNamespace: true, CgroupLeaf: leaf, StartProof: true})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/proc/self/exe" {
		t.Fatalf("path = %q", cmd.Path)
	}
	if len(cmd.Args) < 11 || cmd.Args[1] != imageRuntimeInitArg {
		t.Fatalf("args = %#v", cmd.Args)
	}
	if cmd.Args[2] != "/image" || cmd.Args[3] != "/workspace" || cmd.Args[4] != "1001" || cmd.Args[5] != "1002" || cmd.Args[6] != "true" || cmd.Args[7] != "true" || cmd.Args[8] != leaf || cmd.Args[9] != "true" || cmd.Args[10] != "/usr/bin/node" {
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
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWCGROUP != 0 {
		t.Fatal("Program cgroup namespace was created before cgroup placement")
	}
}

func TestImageCommandPtyUsesSessionWithoutSetpgid(t *testing.T) {
	cmd, err := imageCommand(context.Background(), "/bin/sh", []string{"-l"}, "/workspace", []string{"A=B"}, "/image", &resolvedRuntimeUser{UID: 1001, GID: 1002}, imageCommandOptions{Pty: true})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if cmd.SysProcAttr.Setpgid {
		t.Fatal("PTY command kept Setpgid")
	}
	if cmd.Args[6] != "false" {
		t.Fatalf("managed Program flag = %q", cmd.Args[6])
	}
	if cmd.Args[7] != "false" {
		t.Fatalf("cgroup namespace flag = %q", cmd.Args[7])
	}
	if cmd.Args[8] != "" {
		t.Fatalf("cgroup leaf = %q", cmd.Args[8])
	}
	if cmd.Args[9] != "false" {
		t.Fatalf("start proof flag = %q", cmd.Args[9])
	}
	want := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWPID)
	if cmd.SysProcAttr.Cloneflags&want != want {
		t.Fatalf("clone flags = %#x, want %#x", cmd.SysProcAttr.Cloneflags, want)
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWCGROUP != 0 {
		t.Fatalf("direct PTY received a managed Program cgroup namespace")
	}
}

func TestDefaultImageRuntimeDevicesIncludeStandardProcessDevices(t *testing.T) {
	want := map[string]runtimeDevice{
		"null":    {name: "null", major: 1, minor: 3, mode: 0o666},
		"zero":    {name: "zero", major: 1, minor: 5, mode: 0o666},
		"full":    {name: "full", major: 1, minor: 7, mode: 0o666},
		"random":  {name: "random", major: 1, minor: 8, mode: 0o666},
		"urandom": {name: "urandom", major: 1, minor: 9, mode: 0o666},
		"tty":     {name: "tty", major: 5, minor: 0, mode: 0o666},
	}
	got := make(map[string]runtimeDevice, len(defaultImageRuntimeDevices))
	for _, device := range defaultImageRuntimeDevices {
		got[device.name] = device
	}
	for name, device := range want {
		if got[name] != device {
			t.Fatalf("device %s = %+v, want %+v", name, got[name], device)
		}
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
