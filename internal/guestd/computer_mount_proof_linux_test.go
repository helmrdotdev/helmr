//go:build linux && computerproof

package guestd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestComputerMountProof(t *testing.T) {
	if os.Getenv("HELMR_COMPUTER_MOUNT_PROOF") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestComputerMountProof$", "-test.v")
		cmd.Env = append(os.Environ(), "HELMR_COMPUTER_MOUNT_PROOF=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mount proof: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	root, view := filepath.Join(base, "customer"), filepath.Join(base, "view")
	program, runtime, files := filepath.Join(base, "program"), filepath.Join(base, "runtime"), filepath.Join(base, "files")
	for _, path := range []string{root, view, program, runtime, files} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path, want string) {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("read %s = %q, %v; want %q", path, body, err, want)
		}
	}
	write(filepath.Join(root, "home/agent/history"), "before")
	write(filepath.Join(root, "run/helmr/stale"), "customer reserved bytes")
	write(filepath.Join(root, "var/lib/helmr/stale"), "customer reserved bytes")
	write(filepath.Join(program, "codex"), "pinned executable")
	write(filepath.Join(runtime, "node"), "pinned runtime")
	write(filepath.Join(files, "grant"), "fresh runtime authority")
	if err := os.Symlink("/opt/helmr/program/codex", filepath.Join(root, "home/agent/apply_patch")); err != nil {
		t.Fatal(err)
	}
	cleanup, err := mountComputerRoot(root, view, program, runtime, files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	read(filepath.Join(view, "opt/helmr/program/codex"), "pinned executable")
	read(filepath.Join(view, "run/helmr/grant"), "fresh runtime authority")
	for _, path := range []string{"run/helmr/stale", "var/lib/helmr/stale"} {
		if _, err := os.Stat(filepath.Join(view, path)); !os.IsNotExist(err) {
			t.Fatalf("reserved disk content exposed: %s, %v", path, err)
		}
	}
	for _, path := range []string{"opt/helmr/program/codex", "opt/helmr/runtime/node", "run/helmr/grant", "opt/helmr/untracked", "var/lib/helmr/untracked"} {
		err := os.WriteFile(filepath.Join(view, path), []byte("overwrite"), 0600)
		if !errors.Is(err, unix.EROFS) {
			t.Fatalf("reserved mount is not read-only: %s, %v", path, err)
		}
	}
	// Resolve an absolute link inside the assembled root, not relative to the
	// supervisor's /opt. A static subprocess needs no customer image libraries.
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proof"), binary, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/proof", "-test.run=^TestComputerMountLinkHelper$")
	cmd.Env = []string{"HELMR_COMPUTER_LINK_HELPER=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: view}
	cmd.Dir = "/"
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("root-relative link: %v %s", err, output)
	}
	write(filepath.Join(view, "home/agent/history"), "continued")
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	read(filepath.Join(root, "home/agent/history"), "continued")
	for _, path := range []string{"run/helmr/grant", "opt/helmr/program/codex", "opt/helmr/runtime/node"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("runtime file leaked to disk: %s, %v", path, err)
		}
	}
	// Fresh authority is mounted on a second launch without rewriting customer data.
	write(filepath.Join(files, "grant"), "renewed authority")
	secondCleanup, err := mountComputerRoot(root, view, program, runtime, files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := secondCleanup(); err != nil {
			t.Error(err)
		}
	})
	read(filepath.Join(view, "run/helmr/grant"), "renewed authority")
	read(filepath.Join(view, "home/agent/history"), "continued")
	if err := secondCleanup(); err != nil {
		t.Fatal(err)
	}
	// A persisted symlink must not redirect a reserved mount outside its root.
	if err := os.RemoveAll(filepath.Join(root, "opt/helmr")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(program, filepath.Join(root, "opt/helmr")); err != nil {
		t.Fatal(err)
	}
	rollback, err := mountComputerRoot(root, view, program, runtime, files)
	if err == nil {
		_ = rollback()
		t.Fatal("reserved symlink accepted")
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	read(filepath.Join(program, "codex"), "pinned executable")
	t.Log("durable customer writes, reserved mount exclusion/read-only policy, root-relative absolute link, refreshed authority, and symlink rejection passed")
}

func TestComputerMountLinkHelper(t *testing.T) {
	if os.Getenv("HELMR_COMPUTER_LINK_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	body, err := os.ReadFile("/home/agent/apply_patch")
	if err != nil || string(body) != "pinned executable" {
		t.Fatalf("absolute link = %q, %v", body, err)
	}
}
