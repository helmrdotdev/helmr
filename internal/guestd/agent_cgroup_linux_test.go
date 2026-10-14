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

func TestSessionCgroupRemovedIdentityCannotTargetReplacement(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Session containment test requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	original, err := createSessionProcessCgroup(t.Name(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	if err := os.Remove(original.path); err != nil {
		t.Fatal(err)
	}
	replacement, err := createSessionProcessCgroup(t.Name(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.kill(); _ = replacement.waitEmpty(); _ = replacement.close() }()
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
