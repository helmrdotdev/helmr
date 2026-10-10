//go:build linux

package guestd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNativeCgroupKillPreservesSessionAndOtherNativeProcess(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("set HELMR_PRIVILEGED_PROGRAM_TEST=1 in a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native scope test requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	leaf, err := programCgroupLeafName(t.Name(), 1, "lease")
	if err != nil {
		t.Fatal(err)
	}
	group, err := createProcessCgroup(leaf)
	if err != nil {
		t.Fatal(err)
	}
	parent := group.(*linuxProcessCgroup)
	t.Cleanup(func() { _ = parent.kill(); _ = parent.waitEmpty(); _ = parent.close() })
	first, err := createNativeProcessCgroup(parent, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.file.Close()
	second, err := createNativeProcessCgroup(parent, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.file.Close()
	spawn := func(scope processCgroup) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("/bin/sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := scope.attach(cmd); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	application := spawn(parent)
	native := spawn(first)
	peer := spawn(second)
	if _, err := createNativeProcessCgroup(parent, "first"); err == nil {
		t.Fatal("duplicate scope must not replace a running native process")
	}
	if err := native.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal(err)
	}
	if err := first.kill(); err != nil {
		t.Fatal(err)
	}
	if err := first.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	if err := native.Wait(); err == nil {
		t.Fatal("native process survived scope kill")
	}
	for _, cmd := range []*exec.Cmd{application, peer} {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("native scope kill affected another process: %v", err)
		}
	}
	if err := parent.kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	if err := parent.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(processCgroupRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent scope was not removed: %v", err)
	}
}

func TestEmptyCgroupRemovalDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	marker := filepath.Join(outside, "owned")
	if err := os.WriteFile(marker, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "child")); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyCgroupTree(root); err == nil {
		t.Fatal("accepted a symlink")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retain" {
		t.Fatalf("modified unrelated file: %q, %v", data, err)
	}
}

func TestNativeCgroupRemovedIdentityCannotTargetReplacement(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native scope test requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	leaf, err := programCgroupLeafName(t.Name(), 1, "lease")
	if err != nil {
		t.Fatal(err)
	}
	group, err := createProcessCgroup(leaf)
	if err != nil {
		t.Fatal(err)
	}
	parent := group.(*linuxProcessCgroup)
	defer func() { _ = parent.kill(); _ = parent.waitEmpty(); _ = parent.close() }()
	original, err := createNativeProcessCgroup(parent, "replaced")
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	if err := os.Remove(original.path); err != nil {
		t.Fatal(err)
	}
	replacement, err := createNativeProcessCgroup(parent, "replaced")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.file.Close()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := replacement.attach(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := original.kill(); err == nil {
		t.Fatal("removed scope reported successful kill")
	}
	if err := original.waitEmpty(); err == nil {
		t.Fatal("removed scope reported successful observation")
	}
	if err := original.close(); err == nil {
		t.Fatal("cleanup accepted replacement identity")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("original identity affected replacement: %v", err)
	}
	if _, err := os.Stat(replacement.path); err != nil {
		t.Fatalf("replacement directory removed: %v", err)
	}
}
